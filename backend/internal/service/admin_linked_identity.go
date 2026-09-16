package service

import (
	"context"
	"errors"
	"net/http"

	infraerrors "github.com/luminovaa/neonix-gateway-go/internal/pkg/errors"
)

var ErrLinkedGitHubIdentityNotFound = errors.New("linked GitHub identity not found")

type linkedGitHubIdentityPasswordReader interface {
	RevealLinkedGitHubIdentityPassword(context.Context, int64) (string, error)
}

func (s *adminServiceImpl) RevealLinkedGitHubIdentityPassword(ctx context.Context, codeBuddyAccountID int64) (string, error) {
	if codeBuddyAccountID <= 0 {
		return "", infraerrors.BadRequest("ACCOUNT_ID_INVALID", "invalid account ID")
	}
	account, err := s.accountRepo.GetByID(ctx, codeBuddyAccountID)
	if err != nil {
		return "", err
	}
	if account.Platform != PlatformCodeBuddy {
		return "", infraerrors.BadRequest("LINKED_IDENTITY_PROVIDER_INVALID", "linked identity is only available for CodeBuddy")
	}
	reader, ok := s.accountRepo.(linkedGitHubIdentityPasswordReader)
	if !ok {
		return "", infraerrors.New(http.StatusServiceUnavailable, "LINKED_IDENTITY_UNAVAILABLE", "linked identity storage is unavailable")
	}
	password, err := reader.RevealLinkedGitHubIdentityPassword(ctx, codeBuddyAccountID)
	if errors.Is(err, ErrLinkedGitHubIdentityNotFound) {
		return "", infraerrors.NotFound("LINKED_IDENTITY_NOT_FOUND", "linked GitHub identity not found")
	}
	if err != nil {
		return "", infraerrors.New(http.StatusServiceUnavailable, "LINKED_IDENTITY_UNAVAILABLE", "linked identity storage is unavailable").WithCause(err)
	}
	if password == "" {
		return "", infraerrors.NotFound("LINKED_IDENTITY_NOT_FOUND", "linked GitHub identity not found")
	}
	return password, nil
}
