package domain

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

func validateStringNotEmpty(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%w (%s is empty)", ErrFieldEmpty, field)
	}
	return nil
}

func validateURL(field, value string) error {
	if err := validateStringNotEmpty(field, value); err != nil {
		return err
	}

	toParse := value
	if !strings.Contains(value, "://") {
		toParse = "http://" + value
	}

	parsed, err := url.Parse(toParse)
	if err != nil {
		return fmt.Errorf("%w (%s invalid: %q)", ErrInvalidURL, field, value)
	}
	if parsed.Host == "" {
		return fmt.Errorf("%w (%s missing host)", ErrInvalidURL, field)
	}
	if port := parsed.Port(); port != "" {
		if _, err = strconv.Atoi(port); err != nil {
			return fmt.Errorf("%w (%s invalid port %q)", ErrInvalidURL, field, port)
		}
	}

	switch parsed.Scheme {
	case "http", "https":
		return nil
	default:
		if strings.Contains(value, "://") {
			return fmt.Errorf("%w (%s unsupported scheme %q)", ErrInvalidURL, field, parsed.Scheme)
		}
	}
	return nil
}
