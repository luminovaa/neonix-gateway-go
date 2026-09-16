package routes

import (
	"database/sql"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
)

const maxAccountFilterEmails = 5000

type neonixAccountFilterRequest struct {
	Emails   []string `json:"emails"`
	Provider string   `json:"provider"`
}

func filterRegisteredAccounts(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		if db == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Account store is unavailable", "errorCode": "ACCOUNT_STORE_UNAVAILABLE"})
			return
		}
		var request neonixAccountFilterRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid account filter payload", "errorCode": "ACCOUNT_FILTER_PAYLOAD_INVALID"})
			return
		}
		provider := strings.TrimSpace(request.Provider)
		if provider == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Provider is required", "errorCode": "ACCOUNT_PROVIDER_REQUIRED"})
			return
		}
		emails := normalizedFilterEmails(request.Emails)
		if len(emails) == 0 || len(emails) > maxAccountFilterEmails {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Email list is empty or too large", "errorCode": "ACCOUNT_EMAIL_LIST_INVALID"})
			return
		}

		rows, err := db.QueryContext(c.Request.Context(), `
SELECT lower(extra->>'neonix_legacy_email')
FROM accounts
WHERE deleted_at IS NULL
  AND lower(COALESCE(extra->>'neonix_legacy_provider', extra->>'source_provider', platform)) = lower($1)
  AND lower(extra->>'neonix_legacy_email') = ANY($2)`, provider, pq.Array(emails))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to filter accounts", "errorCode": "ACCOUNT_FILTER_FAILED"})
			return
		}
		defer rows.Close()
		known := make(map[string]struct{}, len(emails))
		for rows.Next() {
			var email string
			if err := rows.Scan(&email); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to filter accounts", "errorCode": "ACCOUNT_FILTER_FAILED"})
				return
			}
			known[strings.ToLower(strings.TrimSpace(email))] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to filter accounts", "errorCode": "ACCOUNT_FILTER_FAILED"})
			return
		}

		registered := make([]string, 0, len(emails))
		unregistered := make([]string, 0, len(emails))
		for _, email := range emails {
			if _, exists := known[email]; exists {
				registered = append(registered, email)
			} else {
				unregistered = append(unregistered, email)
			}
		}
		c.JSON(http.StatusOK, gin.H{"registered": registered, "unregistered": unregistered})
	}
}

func normalizedFilterEmails(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		email := strings.ToLower(strings.TrimSpace(value))
		if email == "" || !strings.Contains(email, "@") {
			continue
		}
		if _, exists := seen[email]; exists {
			continue
		}
		seen[email] = struct{}{}
		result = append(result, email)
	}
	return result
}
