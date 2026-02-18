package providers

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

type credentialCandidate struct {
	mode   string
	source string
	field  string
}

func selectSingleCredential(
	candidates []credentialCandidate,
	missingMessage string,
	multiPrefix string,
) (mode string, source string, err error) {
	switch len(candidates) {
	case 0:
		return "", "", fmt.Errorf("%s", strings.TrimSpace(missingMessage))
	case 1:
		chosen := candidates[0]
		return chosen.mode, chosen.source, nil
	default:
		fields := make([]string, 0, len(candidates))
		for _, item := range candidates {
			fields = append(fields, item.field)
		}
		sort.Strings(fields)
		return "", "", fmt.Errorf(
			"%s (%s); set exactly one",
			strings.TrimSpace(multiPrefix),
			strings.Join(fields, ", "),
		)
	}
}

func validateOAuthTokenFileSource(mode, source, providerLabel string) error {
	if mode != "oauth_token_file" {
		return nil
	}
	resolved := expandHome(strings.TrimSpace(source))
	if _, err := os.Stat(resolved); err != nil {
		label := strings.TrimSpace(providerLabel)
		if label == "" {
			label = "Provider"
		}
		return fmt.Errorf("%s OAuth token file not accessible at %s: %w", label, resolved, err)
	}
	return nil
}
