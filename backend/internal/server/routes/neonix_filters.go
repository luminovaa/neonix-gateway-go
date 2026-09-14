package routes

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/luminovaa/neonix-gateway-go/internal/filterrule"
)

type neonixFilters struct {
	db      *sql.DB
	runtime *filterrule.Runtime
}
type filterInput struct {
	RuleID      *string `json:"ruleId"`
	Pattern     *string `json:"pattern"`
	Replacement *string `json:"replacement"`
	IsActive    *bool   `json:"isActive"`
	IsRegex     *bool   `json:"isRegex"`
	SortOrder   *int    `json:"sortOrder"`
}

func filterError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": message, "errorCode": code})
}
func parseFilterID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		filterError(c, 400, "FILTER_INVALID_ID", "invalid id")
		return 0, false
	}
	return id, true
}
func scanFilter(scanner interface{ Scan(...any) error }) (filterrule.Rule, error) {
	var r filterrule.Rule
	var created time.Time
	var updated sql.NullTime
	err := scanner.Scan(&r.ID, &r.RuleID, &r.Pattern, &r.Replacement, &r.IsActive, &r.IsRegex, &r.SortOrder, &created, &updated)
	r.CreatedAt = created
	if updated.Valid {
		r.UpdatedAt = updated.Time
	} else {
		r.UpdatedAt = nil
	}
	return r, err
}
func (s *neonixFilters) list(c *gin.Context) {
	rows, err := s.db.QueryContext(c, cancelSafeFilterSelect)
	if err != nil {
		filterError(c, 500, "FILTER_LIST_FAILED", "Failed to load filter rules")
		return
	}
	defer rows.Close()
	rules := []filterrule.Rule{}
	active := 0
	for rows.Next() {
		r, e := scanFilter(rows)
		if e != nil {
			filterError(c, 500, "FILTER_LIST_FAILED", "Failed to load filter rules")
			return
		}
		if r.IsActive {
			active++
		}
		rules = append(rules, r)
	}
	if err := rows.Err(); err != nil {
		filterError(c, 500, "FILTER_LIST_FAILED", "Failed to load filter rules")
		return
	}
	c.JSON(200, gin.H{"count": len(rules), "activeCount": active, "rules": rules})
}

const cancelSafeFilterSelect = `SELECT id,rule_id,pattern,replacement,is_active,is_regex,sort_order,created_at,updated_at FROM filter_rules ORDER BY sort_order,id`

func (s *neonixFilters) get(c *gin.Context) {
	id, ok := parseFilterID(c)
	if !ok {
		return
	}
	r, err := scanFilter(s.db.QueryRowContext(c, idSelect, id))
	if errors.Is(err, sql.ErrNoRows) {
		filterError(c, 404, "FILTER_RULE_NOT_FOUND", "rule not found")
		return
	}
	if err != nil {
		filterError(c, 500, "FILTER_LOAD_FAILED", "Failed to load filter rule")
		return
	}
	c.JSON(200, r)
}

const idSelect = `SELECT id,rule_id,pattern,replacement,is_active,is_regex,sort_order,created_at,updated_at FROM filter_rules WHERE id=$1`

func validateFilterInput(in filterInput, create bool) (string, string, bool, error) {
	pattern := ""
	if in.Pattern != nil {
		pattern = *in.Pattern
	}
	if create && in.Pattern == nil {
		return "", "", false, fmt.Errorf("pattern required")
	}
	replacement := ""
	if in.Replacement != nil {
		replacement = *in.Replacement
	}
	regex := false
	if in.IsRegex != nil {
		regex = *in.IsRegex
	}
	if in.Pattern != nil {
		if err := filterrule.Validate(pattern, replacement, regex); err != nil {
			return pattern, replacement, regex, err
		}
	}
	return pattern, replacement, regex, nil
}
func (s *neonixFilters) create(c *gin.Context) {
	var in filterInput
	if c.ShouldBindJSON(&in) != nil {
		filterError(c, 400, "FILTER_PAYLOAD_INVALID", "invalid payload")
		return
	}
	pattern, repl, regex, err := validateFilterInput(in, true)
	if err != nil {
		code := "FILTER_PATTERN_INVALID"
		if strings.TrimSpace(pattern) == "" {
			code = "FILTER_PATTERN_REQUIRED"
		}
		filterError(c, 400, code, err.Error())
		return
	}
	ruleID := fmt.Sprintf("rule_%d", time.Now().UnixNano())
	if in.RuleID != nil && strings.TrimSpace(*in.RuleID) != "" {
		ruleID = strings.TrimSpace(*in.RuleID)
	}
	active := true
	if in.IsActive != nil {
		active = *in.IsActive
	}
	order := 0
	if in.SortOrder != nil {
		order = *in.SortOrder
	}
	r, err := scanFilter(s.db.QueryRowContext(c, `INSERT INTO filter_rules(rule_id,pattern,replacement,is_active,is_regex,sort_order) VALUES($1,$2,$3,$4,$5,$6) RETURNING id,rule_id,pattern,replacement,is_active,is_regex,sort_order,created_at,updated_at`, ruleID, pattern, repl, active, regex, order))
	if err != nil {
		filterError(c, 500, "FILTER_SAVE_FAILED", "Failed to save filter rule")
		return
	}
	if err := s.runtime.Reload(); err != nil {
		filterError(c, 500, "FILTER_CACHE_REFRESH_FAILED", "Filter rule was saved but runtime refresh failed")
		return
	}
	c.JSON(201, r)
}
func (s *neonixFilters) update(c *gin.Context) {
	id, ok := parseFilterID(c)
	if !ok {
		return
	}
	var in filterInput
	if c.ShouldBindJSON(&in) != nil {
		filterError(c, 400, "FILTER_PAYLOAD_INVALID", "invalid payload")
		return
	}
	current, err := scanFilter(s.db.QueryRowContext(c, idSelect, id))
	if errors.Is(err, sql.ErrNoRows) {
		filterError(c, 404, "FILTER_RULE_NOT_FOUND", "rule not found")
		return
	}
	if err != nil {
		filterError(c, 500, "FILTER_LOAD_FAILED", "Failed to load filter rule")
		return
	}
	effectivePattern, effectiveReplacement, effectiveRegex := current.Pattern, current.Replacement, current.IsRegex
	if in.Pattern != nil {
		effectivePattern = *in.Pattern
	}
	if in.Replacement != nil {
		effectiveReplacement = *in.Replacement
	}
	if in.IsRegex != nil {
		effectiveRegex = *in.IsRegex
	}
	err = filterrule.Validate(effectivePattern, effectiveReplacement, effectiveRegex)
	if err != nil {
		code := "FILTER_PATTERN_INVALID"
		if strings.TrimSpace(effectivePattern) == "" {
			code = "FILTER_PATTERN_REQUIRED"
		}
		filterError(c, 400, code, err.Error())
		return
	}
	repl, regex := effectiveReplacement, effectiveRegex
	var r filterrule.Rule
	err = s.db.QueryRowContext(c, `UPDATE filter_rules SET rule_id=COALESCE($2,rule_id),pattern=COALESCE($3,pattern),replacement=COALESCE($4,replacement),is_active=COALESCE($5,is_active),is_regex=COALESCE($6,is_regex),sort_order=COALESCE($7,sort_order),updated_at=NOW() WHERE id=$1 RETURNING id,rule_id,pattern,replacement,is_active,is_regex,sort_order,created_at,updated_at`, id, in.RuleID, in.Pattern, nullableString(in.Replacement, repl), in.IsActive, nullableBool(in.IsRegex, regex), in.SortOrder).Scan(&r.ID, &r.RuleID, &r.Pattern, &r.Replacement, &r.IsActive, &r.IsRegex, &r.SortOrder, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		filterError(c, 404, "FILTER_RULE_NOT_FOUND", "rule not found")
		return
	}
	if err != nil {
		filterError(c, 500, "FILTER_SAVE_FAILED", "Failed to save filter rule")
		return
	}
	if err := s.runtime.Reload(); err != nil {
		filterError(c, 500, "FILTER_CACHE_REFRESH_FAILED", "Filter rule was saved but runtime refresh failed")
		return
	}
	c.JSON(200, r)
}
func nullableString(p *string, v string) any {
	if p == nil {
		return nil
	}
	return v
}
func nullableBool(p *bool, v bool) any {
	if p == nil {
		return nil
	}
	return v
}
func (s *neonixFilters) delete(c *gin.Context) {
	id, ok := parseFilterID(c)
	if !ok {
		return
	}
	res, err := s.db.ExecContext(c, `DELETE FROM filter_rules WHERE id=$1`, id)
	if err != nil {
		filterError(c, 500, "FILTER_DELETE_FAILED", "Failed to delete filter rule")
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		filterError(c, 404, "FILTER_RULE_NOT_FOUND", "rule not found")
		return
	}
	if err := s.runtime.Reload(); err != nil {
		filterError(c, 500, "FILTER_CACHE_REFRESH_FAILED", "Filter rule was deleted but runtime refresh failed")
		return
	}
	c.JSON(200, gin.H{"ok": true})
}
