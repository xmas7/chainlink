package v2

import (
	"fmt"
	"strings"

	"go.uber.org/multierr"

	"github.com/smartcontractkit/chainlink/core/utils"
)

// Validated configurations impose constraints that must be checked.
type Validated interface {
	// ValidateConfig returns nil if the config is valid, otherwise an error describing why it is invalid.
	//
	// For implementations:
	//  - A nil receiver should return nil, freeing the caller to decide whether each case is required.
	//  - Use package multierr to accumulate all errors, rather than returning the first encountered.
	ValidateConfig() error
}

// Validate is a helper to append cfg validation errors to err, as well as deocration them with name and indentation.
func Validate(err error, cfg Validated, name string) error {
	if err2 := cfg.ValidateConfig(); err2 != nil {
		err2 = utils.MultiErrorList(err2)
		msg := strings.ReplaceAll(err2.Error(), "\n", "\n\t")
		return multierr.Append(err, fmt.Errorf("%s: %s", name, msg))
	}
	return err
}

//TODO use this
type ErrInvalid struct {
	Name  string
	Value any
	Msg   string
}

func (e ErrInvalid) Error() string {
	return fmt.Sprintf("%s: invalid: %v: %s", e.Name, e.Value, e.Msg)
}

type ErrMissing struct {
	Name string
	Msg  string
}

func (e ErrMissing) Error() string {
	return fmt.Sprintf("%s: missing: %s", e.Name, e.Msg)
}
