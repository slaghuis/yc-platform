package errors

// ValidationBuilder accumulates field errors for a validation_failed error.
//
//	v := errors.Validation()
//	if req.Email == "" {
//	    v.Field("email", "required", "Email is required")
//	}
//	if v.HasErrors() {
//	    return v.Build()
//	}
type ValidationBuilder struct {
	message string
	fields  []FieldError
}

// Validation starts a validation_failed error.
func Validation() *ValidationBuilder {
	return &ValidationBuilder{message: "Validation failed"}
}

// Message overrides the top-level user-safe message.
func (v *ValidationBuilder) Message(m string) *ValidationBuilder {
	v.message = m
	return v
}

// Field records one field-level failure.
func (v *ValidationBuilder) Field(field, code, message string) *ValidationBuilder {
	v.fields = append(v.fields, FieldError{Field: field, Code: code, Message: message})
	return v
}

// HasErrors reports whether any field errors were recorded.
func (v *ValidationBuilder) HasErrors() bool { return len(v.fields) > 0 }

// Build returns the accumulated error.
func (v *ValidationBuilder) Build() *Error {
	fields := make([]FieldError, len(v.fields))
	copy(fields, v.fields)
	return &Error{
		Code:    CodeValidationFailed,
		Status:  CodeValidationFailed.Status(),
		Message: v.message,
		Fields:  fields,
	}
}

// Wrap builds the error with cause attached, for malformed input such as
// undecodable JSON.
func (v *ValidationBuilder) Wrap(cause error) *Error {
	return v.Build().Wrap(cause)
}
