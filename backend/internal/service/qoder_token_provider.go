package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/qoder"
	"golang.org/x/sync/singleflight"
)

const (
	qoderJobTokenURL     = "https://center.qoder.sh/algo/api/v3/user/jobToken?Encode=1"
	qoderCosyVersion     = "0.1.43"
	qoderAppCode         = "cosy"
	qoderSignatureSecret = "d2FyLCB3YXIgbmV2ZXIgY2hhbmdlcw=="
)

type QoderTokens struct {
	PersonalToken, SecurityOAuthToken, RefreshToken string
	UserID, UserName, UserType, Plan, Email         string
	MachineID, MachineToken, MachineType            string
	ExpiresAt                                       time.Time
}

type qoderTokenCacheEntry struct {
	tokens              QoderTokens
	expiresAt           time.Time
	credentialSignature [32]byte
}

type qoderTokenExchangeError struct {
	status int
	header http.Header
	err    error
}

func (e *qoderTokenExchangeError) Error() string {
	if e == nil || e.err == nil {
		return "Qoder token exchange failed"
	}
	return e.err.Error()
}
func (e *qoderTokenExchangeError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

type QoderTokenProvider struct {
	accountRepo  AccountRepository
	httpUpstream HTTPUpstream
	now          func() time.Time
	group        singleflight.Group
	mu           sync.Mutex
	cache        map[int64]qoderTokenCacheEntry
}

func NewQoderTokenProvider(repo AccountRepository, upstream HTTPUpstream) *QoderTokenProvider {
	return &QoderTokenProvider{accountRepo: repo, httpUpstream: upstream, now: time.Now, cache: make(map[int64]qoderTokenCacheEntry)}
}

func (p *QoderTokenProvider) Tokens(ctx context.Context, account *Account) (QoderTokens, error) {
	if account == nil || account.Platform != PlatformQoder {
		return QoderTokens{}, errors.New("Qoder account is required")
	}
	personal := qoderCredential(account, "token", "accessToken", "access_token", "apiKey", "api_key", "authToken", "auth_token")
	if personal == "" {
		return QoderTokens{}, errors.New("Qoder personal access token is missing")
	}
	signature := sha256.Sum256([]byte(personal))
	p.mu.Lock()
	cached, ok := p.cache[account.ID]
	p.mu.Unlock()
	if ok && cached.credentialSignature == signature && p.now().Add(time.Minute).Before(cached.expiresAt) {
		return cached.tokens, nil
	}
	groupKey := strconv.FormatInt(account.ID, 10) + ":" + hex.EncodeToString(signature[:8])
	value, err, _ := p.group.Do(groupKey, func() (any, error) { return p.exchange(ctx, account, signature) })
	if err != nil {
		return QoderTokens{}, err
	}
	return value.(QoderTokens), nil
}

func (p *QoderTokenProvider) exchange(ctx context.Context, account *Account, credentialSignature [32]byte) (QoderTokens, error) {
	personal := qoderCredential(account, "token", "accessToken", "access_token", "apiKey", "api_key", "authToken", "auth_token")
	if personal == "" {
		return QoderTokens{}, errors.New("Qoder personal access token is missing")
	}
	machineID := qoderCredential(account, "machineId", "machine_id")
	if machineID == "" {
		machineID = stableQoderMachineID(account)
	}
	hash := sha256.Sum256([]byte(machineID))
	hashHex := hex.EncodeToString(hash[:])
	machineToken := base64.RawURLEncoding.EncodeToString([]byte((hashHex + strings.ReplaceAll(machineID, "-", ""))[:50]))
	seed := QoderTokens{PersonalToken: personal, RefreshToken: qoderCredential(account, "refreshToken", "refresh_token"), MachineID: machineID, MachineToken: machineToken, MachineType: hashHex[:18]}
	inner := map[string]any{"personalToken": seed.PersonalToken, "securityOauthToken": "", "refreshToken": seed.RefreshToken, "needRefresh": seed.RefreshToken != "", "authInfo": map[string]any{}}
	payloadJSON, _ := json.Marshal(inner)
	body, err := qoder.EncodePayload(map[string]any{"payload": string(payloadJSON), "encodeVersion": "1"})
	if err != nil {
		return QoderTokens{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, qoderJobTokenURL, bytes.NewBufferString(body))
	if err != nil {
		return QoderTokens{}, err
	}
	date := p.now().UTC().Format(http.TimeFormat)
	for k, v := range qoderSignedHeaders(seed, date) {
		req.Header.Set(k, v)
	}
	resp, err := p.do(ctx, account, req)
	if err != nil {
		return QoderTokens{}, &qoderTokenExchangeError{status: http.StatusBadGateway, err: fmt.Errorf("Qoder job-token transport failed: %w", err)}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return QoderTokens{}, errors.New("Qoder job-token response could not be read")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return QoderTokens{}, &qoderTokenExchangeError{status: resp.StatusCode, header: resp.Header.Clone(), err: fmt.Errorf("Qoder job-token exchange rejected with HTTP %d", resp.StatusCode)}
	}
	var out struct {
		ID, Name, SecurityOAuthToken, RefreshToken, Email, Plan, UserType string
		ExpireTime                                                        int64 `json:"expireTime"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return QoderTokens{}, errors.New("Qoder job-token response was invalid")
	}
	if out.ID == "" {
		return QoderTokens{}, errors.New("Qoder job-token response did not contain a user identity")
	}
	if strings.TrimSpace(out.SecurityOAuthToken) == "" {
		return QoderTokens{}, errors.New("Qoder job-token response did not contain an access token")
	}
	expires := qoderExpiry(out.ExpireTime, p.now())
	tokens := seed
	tokens.SecurityOAuthToken = out.SecurityOAuthToken
	tokens.UserID = out.ID
	tokens.UserName = out.Name
	tokens.UserType = out.UserType
	if tokens.UserType == "" {
		tokens.UserType = "personal_standard"
	}
	tokens.Plan = out.Plan
	tokens.Email = out.Email
	tokens.ExpiresAt = expires
	if out.RefreshToken != "" {
		tokens.RefreshToken = out.RefreshToken
	}
	credentials := shallowCopyMap(account.Credentials)
	setQoderCredential(credentials, account.Credentials, "refreshToken", "refresh_token", tokens.RefreshToken)
	setQoderCredential(credentials, account.Credentials, "machineId", "machine_id", tokens.MachineID)
	setQoderCredential(credentials, account.Credentials, "userId", "user_id", tokens.UserID)
	setQoderCredential(credentials, account.Credentials, "expiresAt", "expires_at", tokens.ExpiresAt.UnixMilli())
	if err := persistAccountCredentials(ctx, p.accountRepo, account, credentials); err != nil {
		return QoderTokens{}, fmt.Errorf("persist Qoder token metadata: %w", err)
	}
	if p.accountRepo == nil {
		account.Credentials = credentials
	}
	p.mu.Lock()
	p.cache[account.ID] = qoderTokenCacheEntry{tokens: tokens, expiresAt: expires, credentialSignature: credentialSignature}
	p.mu.Unlock()
	return tokens, nil
}
func (p *QoderTokenProvider) do(_ context.Context, account *Account, req *http.Request) (*http.Response, error) {
	if p.httpUpstream == nil {
		return nil, errors.New("Qoder HTTP upstream is not configured")
	}
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	return p.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
}
func qoderSignedHeaders(tokens QoderTokens, date string) map[string]string {
	return map[string]string{"cosy-machinetoken": tokens.MachineToken, "cosy-machinetype": tokens.MachineType, "login-version": "v2", "appcode": qoderAppCode, "accept": "application/json", "accept-encoding": "identity", "cosy-version": qoderCosyVersion, "cosy-clienttype": "5", "date": date, "signature": qoder.MD5Hex(qoderAppCode + "&" + qoderSignatureSecret + "&" + date), "content-type": "application/json", "cosy-machineid": tokens.MachineID, "user-agent": "Go-http-client/2.0"}
}
func stableQoderMachineID(account *Account) string {
	seed := strconv.FormatInt(account.ID, 10)
	if account.Name != "" {
		seed += ":" + account.Name
	}
	sum := sha256.Sum256([]byte(seed))
	id, _ := uuid.FromBytes(sum[:16])
	return id.String()
}
func qoderExpiry(value int64, now time.Time) time.Time {
	if value <= 0 {
		return now.Add(3 * time.Hour)
	}
	if value < 10_000_000_000 {
		return time.Unix(value, 0)
	}
	return time.UnixMilli(value)
}
func qoderCredential(account *Account, keys ...string) string {
	if account == nil {
		return ""
	}
	for _, key := range keys {
		if value := strings.TrimSpace(account.GetCredential(key)); value != "" {
			return value
		}
	}
	return ""
}
func setQoderCredential(dst, original map[string]any, camel, snake string, value any) {
	if text, ok := value.(string); ok && text == "" {
		return
	}
	key := camel
	if _, ok := original[snake]; ok {
		key = snake
	}
	dst[key] = value
}
