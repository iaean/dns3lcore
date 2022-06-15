package common

import (
	"errors"
	"regexp"
)

var keyNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

func ValidateKeyName(key string) error {
	if keyNameRe.MatchString(key) {
		return nil
	}
	return errors.New("key_name provided has invalid format or is too long")
}
