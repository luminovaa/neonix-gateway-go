package filterrule

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"

	"github.com/gin-gonic/gin"
)

const (
	MaxPatternLength     = 8192
	MaxReplacementLength = 32768
)

type Rule struct {
	ID          int64  `json:"id"`
	RuleID      string `json:"ruleId"`
	Pattern     string `json:"pattern"`
	Replacement string `json:"replacement"`
	IsActive    bool   `json:"isActive"`
	IsRegex     bool   `json:"isRegex"`
	SortOrder   int    `json:"sortOrder"`
	CreatedAt   any    `json:"createdAt"`
	UpdatedAt   any    `json:"updatedAt"`
}

type compiledRule struct {
	pattern, replacement string
	regex                *regexp.Regexp
}
type Runtime struct {
	db       *sql.DB
	snapshot atomic.Pointer[[]compiledRule]
}

var current atomic.Pointer[Runtime]

func New(db *sql.DB) *Runtime {
	r := &Runtime{db: db}
	empty := []compiledRule{}
	r.snapshot.Store(&empty)
	return r
}
func SetCurrent(r *Runtime) { current.Store(r) }
func Current() *Runtime     { return current.Load() }

func Validate(pattern, replacement string, isRegex bool) error {
	if strings.TrimSpace(pattern) == "" {
		return fmt.Errorf("pattern is required")
	}
	if len(pattern) > MaxPatternLength {
		return fmt.Errorf("pattern is too long")
	}
	if len(replacement) > MaxReplacementLength {
		return fmt.Errorf("replacement is too long")
	}
	if isRegex {
		if _, err := regexp.Compile("(?i)" + pattern); err != nil {
			return fmt.Errorf("invalid regular expression: %w", err)
		}
	}
	return nil
}

func (r *Runtime) Reload() error {
	if r == nil || r.db == nil {
		return fmt.Errorf("filter store unavailable")
	}
	rows, err := r.db.Query(`SELECT pattern,replacement,is_regex FROM filter_rules WHERE is_active=TRUE ORDER BY sort_order,id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	next := make([]compiledRule, 0)
	for rows.Next() {
		var cr compiledRule
		var isRegex bool
		if err := rows.Scan(&cr.pattern, &cr.replacement, &isRegex); err != nil {
			return err
		}
		if isRegex {
			cr.regex, err = regexp.Compile("(?i)" + cr.pattern)
			if err != nil {
				return err
			}
		}
		next = append(next, cr)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	r.snapshot.Store(&next)
	return nil
}

func (r *Runtime) Apply(value string) string {
	if r == nil || value == "" {
		return value
	}
	rules := r.snapshot.Load()
	if rules == nil {
		return value
	}
	for _, rule := range *rules {
		if rule.regex != nil {
			value = rule.regex.ReplaceAllString(value, rule.replacement)
		} else {
			value = strings.ReplaceAll(value, rule.pattern, rule.replacement)
		}
	}
	return value
}

// FilterRequestJSON mirrors the legacy scope: user/system message text only.
// Tool schemas, model IDs, URLs, binary parts, and response bytes are untouched.
func FilterRequestJSON(body []byte) []byte {
	r := Current()
	if r == nil || len(body) == 0 {
		return body
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var root map[string]any
	if dec.Decode(&root) != nil {
		return body
	}
	changed := false
	filterContent := func(value any) any { return value }
	var walkContent func(any) any
	walkContent = func(value any) any {
		switch v := value.(type) {
		case string:
			n := r.Apply(v)
			if n != v {
				changed = true
			}
			return n
		case []any:
			for i, part := range v {
				if obj, ok := part.(map[string]any); ok {
					if typ, _ := obj["type"].(string); typ == "text" || typ == "input_text" {
						if text, ok := obj["text"].(string); ok {
							n := r.Apply(text)
							if n != text {
								changed = true
							}
							obj["text"] = n
						}
					}
					v[i] = obj
				}
			}
			return v
		}
		return value
	}
	filterContent = walkContent
	if system, ok := root["system"]; ok {
		root["system"] = filterContent(system)
	}
	if messages, ok := root["messages"].([]any); ok {
		for _, item := range messages {
			if msg, ok := item.(map[string]any); ok {
				if content, exists := msg["content"]; exists {
					msg["content"] = filterContent(content)
				}
			}
		}
	}
	if input, ok := root["input"].([]any); ok {
		for _, item := range input {
			if msg, ok := item.(map[string]any); ok {
				if content, exists := msg["content"]; exists {
					msg["content"] = filterContent(content)
				}
			}
		}
	}
	if !changed {
		return body
	}
	encoded, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return encoded
}

func Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Body == nil || !strings.Contains(strings.ToLower(c.GetHeader("Content-Type")), "json") {
			c.Next()
			return
		}
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "message": "Failed to read request body"}})
			return
		}
		body = FilterRequestJSON(body)
		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		c.Request.ContentLength = int64(len(body))
		c.Next()
	}
}
