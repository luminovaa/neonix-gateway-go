package admin

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	infraerrors "github.com/luminovaa/neonix-gateway-go/internal/pkg/errors"
	"github.com/luminovaa/neonix-gateway-go/internal/pkg/response"
	"github.com/luminovaa/neonix-gateway-go/internal/service"
)

type neonixProxyPoolUpdateRequest struct {
	Enabled        *bool   `json:"enabled"`
	CustomProtocol string  `json:"customProtocol"`
	CustomProxies  *string `json:"customProxies"`
}

type neonixProxyPoolEntryUpdateRequest struct {
	Enabled *bool `json:"enabled" binding:"required"`
}

type neonixProxyPoolEntry struct {
	ID      int64   `json:"id"`
	Proxy   string  `json:"proxy"`
	Port    int     `json:"port"`
	Enabled bool    `json:"enabled"`
	Online  *bool   `json:"online"`
	IP      *string `json:"ip"`
}

type neonixProxyPoolStatus struct {
	Current            *string                `json:"current"`
	CurrentOnline      *bool                  `json:"currentOnline"`
	CurrentIP          *string                `json:"currentIp"`
	UsedCount          int64                  `json:"usedCount"`
	Enabled            bool                   `json:"enabled"`
	LastError          *string                `json:"lastError"`
	CustomProtocol     string                 `json:"customProtocol"`
	CustomProxies      string                 `json:"customProxies"`
	CustomProxiesCount int                    `json:"customProxiesCount"`
	ActiveProxies      []neonixProxyPoolEntry `json:"activeProxies"`
}

type neonixProxyTestResult struct {
	Proxy     string  `json:"proxy"`
	OK        bool    `json:"ok"`
	LatencyMs int64   `json:"latencyMs"`
	Status    *int    `json:"status"`
	IP        *string `json:"ip"`
	Error     *string `json:"error"`
}

// CustomPoolStatusCompat projects the canonical Go proxy records into the
// direct JSON contract used by the Neonix Proxy Pool page. Authentication
// material is deliberately omitted from every projected URL.
func (h *ProxyHandler) CustomPoolStatusCompat(c *gin.Context) {
	status, err := h.neonixProxyPoolStatus(c)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, status)
}

// UpdateCustomPoolCompat imports a replacement set of active proxies. Existing
// records are reactivated by host and port so their stored credentials remain
// intact; omitted active records are made inactive instead of being deleted.
func (h *ProxyHandler) UpdateCustomPoolCompat(c *gin.Context) {
	var req neonixProxyPoolUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid proxy pool configuration")
		return
	}
	if req.Enabled != nil {
		if err := h.setNeonixProxyPoolEnabled(c, *req.Enabled); err != nil {
			response.ErrorFrom(c, err)
			return
		}
	}
	if req.CustomProxies != nil {
		if err := h.replaceNeonixActiveProxies(c, *req.CustomProxies, req.CustomProtocol); err != nil {
			response.ErrorFrom(c, err)
			return
		}
	}
	status, err := h.neonixProxyPoolStatus(c)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, status)
}

// UpdateCustomPoolEntryCompat changes only the canonical proxy status. The
// stored address and authentication material are left untouched, so an
// operator can temporarily remove a proxy from scheduling and restore it
// later without entering its credentials again.
func (h *ProxyHandler) UpdateCustomPoolEntryCompat(c *gin.Context) {
	proxyID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || proxyID < 1 {
		response.BadRequest(c, "invalid proxy ID")
		return
	}
	var req neonixProxyPoolEntryUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.Enabled == nil {
		response.BadRequest(c, "enabled is required")
		return
	}
	status := service.StatusDisabled
	if *req.Enabled {
		status = service.StatusActive
	}
	if _, err := h.adminService.UpdateProxy(c.Request.Context(), proxyID, &service.UpdateProxyInput{Status: status}); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	poolStatus, err := h.neonixProxyPoolStatus(c)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"ok": true, "status": poolStatus})
}

func (h *ProxyHandler) CustomPoolActionCompat(c *gin.Context) {
	status, err := h.neonixProxyPoolStatus(c)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"ok": true, "status": status})
}

func (h *ProxyHandler) TestCustomPoolCompat(c *gin.Context) {
	var req struct {
		Proxy string `json:"proxy"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid proxy test request")
		return
	}
	proxies, err := h.adminService.GetAllProxies(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	targets := proxies
	if strings.TrimSpace(req.Proxy) != "" {
		host, port, parseErr := neonixProxyAddress(req.Proxy, "http")
		if parseErr != nil {
			response.BadRequest(c, "invalid proxy address")
			return
		}
		targets = nil
		for i := range proxies {
			if strings.EqualFold(proxies[i].Host, host) && proxies[i].Port == port {
				targets = append(targets, proxies[i])
				break
			}
		}
		if len(targets) == 0 {
			response.NotFound(c, "proxy not found")
			return
		}
	}

	results := make([]neonixProxyTestResult, 0, len(targets))
	succeeded := 0
	for i := range targets {
		proxy := &targets[i]
		result, testErr := h.adminService.TestProxy(c.Request.Context(), proxy.ID)
		item := neonixProxyTestResult{Proxy: neonixSafeProxyURL(proxy)}
		if testErr == nil && result != nil && result.Success {
			item.OK = true
			item.LatencyMs = result.LatencyMs
			if result.IPAddress != "" {
				item.IP = &result.IPAddress
			}
			succeeded++
		} else {
			message := "proxy connection failed"
			item.Error = &message
		}
		results = append(results, item)
	}
	status, err := h.neonixProxyPoolStatus(c)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{
		"testUrl": "server-managed connectivity probe",
		"tested":  len(results), "succeeded": succeeded,
		"failed": len(results) - succeeded, "results": results, "status": status,
	})
}

func (h *ProxyHandler) neonixProxyPoolStatus(c *gin.Context) (*neonixProxyPoolStatus, error) {
	all, _, err := h.adminService.ListProxies(c.Request.Context(), 1, 10000, "", "", "", "id", "asc")
	if err != nil {
		return nil, err
	}
	proxies, err := h.adminService.GetAllProxiesWithAccountCount(c.Request.Context())
	if err != nil {
		return nil, err
	}
	status := &neonixProxyPoolStatus{
		Enabled: len(proxies) > 0, CustomProtocol: "http",
		ActiveProxies: make([]neonixProxyPoolEntry, 0, len(all)),
	}
	activeByID := make(map[int64]*service.ProxyWithAccountCount, len(proxies))
	for i := range proxies {
		activeByID[proxies[i].ID] = &proxies[i]
	}
	lines := make([]string, 0, len(all))
	for i := range all {
		proxy := &all[i]
		safeURL := neonixSafeProxyURL(proxy)
		entry := neonixProxyPoolEntry{ID: proxy.ID, Proxy: safeURL, Port: proxy.Port, Enabled: proxy.Status == service.StatusActive}
		if active := activeByID[proxy.ID]; active != nil {
			if active.LatencyStatus != "" {
				online := active.LatencyStatus == "success"
				entry.Online = &online
			}
			if active.IPAddress != "" {
				entry.IP = &active.IPAddress
			}
			status.UsedCount += active.AccountCount
		} else {
			online := false
			entry.Online = &online
		}
		status.ActiveProxies = append(status.ActiveProxies, entry)
		lines = append(lines, safeURL)
	}
	status.CustomProxies = strings.Join(lines, "\n")
	status.CustomProxiesCount = len(lines)
	for i := range status.ActiveProxies {
		if activeByID[status.ActiveProxies[i].ID] != nil {
			status.Current = &status.ActiveProxies[i].Proxy
			status.CurrentOnline = status.ActiveProxies[i].Online
			status.CurrentIP = status.ActiveProxies[i].IP
			break
		}
	}
	return status, nil
}

func (h *ProxyHandler) setNeonixProxyPoolEnabled(c *gin.Context, enabled bool) error {
	all, _, err := h.adminService.ListProxies(c.Request.Context(), 1, 10000, "", "", "", "id", "asc")
	if err != nil {
		return err
	}
	target := service.StatusDisabled
	if enabled {
		target = service.StatusActive
	}
	for i := range all {
		if all[i].Status == target {
			continue
		}
		if _, err := h.adminService.UpdateProxy(c.Request.Context(), all[i].ID, &service.UpdateProxyInput{Status: target}); err != nil {
			return err
		}
	}
	return nil
}

func (h *ProxyHandler) replaceNeonixActiveProxies(c *gin.Context, raw, defaultProtocol string) error {
	if defaultProtocol == "" {
		defaultProtocol = "http"
	}
	all, _, err := h.adminService.ListProxies(c.Request.Context(), 1, 10000, "", "", "", "id", "asc")
	if err != nil {
		return err
	}
	type parsedProxy struct {
		protocol, host, username, password string
		port                               int
	}
	desired := make(map[string]parsedProxy)
	lines := strings.Split(raw, "\n")
	if len(lines) > 1000 {
		return infraerrors.BadRequest("PROXY_POOL_TOO_LARGE", "proxy pool cannot exceed 1000 lines")
	}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if len(line) > 2048 {
			return infraerrors.BadRequest("PROXY_ADDRESS_INVALID", "proxy address is too long")
		}
		protocol, host, port, username, password, parseErr := parseNeonixProxy(line, defaultProtocol)
		if parseErr != nil {
			return infraerrors.BadRequest("PROXY_ADDRESS_INVALID", parseErr.Error())
		}
		desired[neonixProxyKey(host, port)] = parsedProxy{protocol, host, username, password, port}
	}
	existing := make(map[string]*service.Proxy, len(all))
	for i := range all {
		existing[neonixProxyKey(all[i].Host, all[i].Port)] = &all[i]
	}
	for key, parsed := range desired {
		if current, ok := existing[key]; ok {
			if current.Status != service.StatusActive {
				_, err = h.adminService.UpdateProxy(c.Request.Context(), current.ID, &service.UpdateProxyInput{Status: service.StatusActive})
				if err != nil {
					return err
				}
			}
			continue
		}
		_, err = h.adminService.CreateProxy(c.Request.Context(), &service.CreateProxyInput{
			Name: fmt.Sprintf("Proxy %s:%d", parsed.host, parsed.port), Protocol: parsed.protocol,
			Host: parsed.host, Port: parsed.port, Username: parsed.username, Password: parsed.password,
		})
		if err != nil {
			return err
		}
	}
	for i := range all {
		if all[i].Status == service.StatusActive {
			if _, keep := desired[neonixProxyKey(all[i].Host, all[i].Port)]; !keep {
				_, err = h.adminService.UpdateProxy(c.Request.Context(), all[i].ID, &service.UpdateProxyInput{Status: service.StatusDisabled})
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func parseNeonixProxy(raw, defaultProtocol string) (protocol, host string, port int, username, password string, err error) {
	value := strings.TrimSpace(raw)
	if !strings.Contains(value, "://") {
		parts := strings.Split(value, ":")
		if len(parts) == 4 {
			value = fmt.Sprintf("%s://%s:%s@%s:%s", defaultProtocol, url.QueryEscape(parts[2]), url.QueryEscape(parts[3]), parts[0], parts[1])
		} else {
			value = defaultProtocol + "://" + value
		}
	}
	parsed, parseErr := url.Parse(value)
	if parseErr != nil || parsed.Hostname() == "" || parsed.Port() == "" {
		err = fmt.Errorf("invalid proxy address")
		return
	}
	protocol = strings.ToLower(parsed.Scheme)
	if protocol != "http" && protocol != "https" && protocol != "socks5" && protocol != "socks5h" {
		err = fmt.Errorf("unsupported proxy protocol")
		return
	}
	port, parseErr = strconv.Atoi(parsed.Port())
	if parseErr != nil || port < 1 || port > 65535 {
		err = fmt.Errorf("invalid proxy port")
		return
	}
	host = strings.ToLower(parsed.Hostname())
	if parsed.User != nil {
		username = parsed.User.Username()
		password, _ = parsed.User.Password()
	}
	return
}

func neonixProxyAddress(raw, defaultProtocol string) (string, int, error) {
	_, host, port, _, _, err := parseNeonixProxy(raw, defaultProtocol)
	return host, port, err
}

func neonixProxyKey(host string, port int) string {
	return strings.ToLower(strings.TrimSpace(host)) + ":" + strconv.Itoa(port)
}

func neonixSafeProxyURL(proxy *service.Proxy) string {
	return proxy.Protocol + "://" + net.JoinHostPort(proxy.Host, strconv.Itoa(proxy.Port))
}
