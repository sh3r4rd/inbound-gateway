package gateway

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"github.com/go-playground/validator/v10"
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/checker/decls"
	"go.uber.org/zap"
)

// RequestValidator handles request validation
type RequestValidator struct {
	config      *Config
	logger      *zap.Logger
	validator   *validator.Validate
	celEnv      *cel.Env
	customFuncs map[string]ValidationFunc
}

// ValidationFunc is a custom validation function
type ValidationFunc func(value interface{}, rule FieldValidator) error

// ValidationError represents a validation error
type ValidationError struct {
	Field   string      `json:"field"`
	Value   interface{} `json:"value,omitempty"`
	Message string      `json:"message"`
	Code    string      `json:"code"`
}

func (e ValidationError) Error() string {
	return fmt.Sprintf("validation failed for field %s: %s", e.Field, e.Message)
}

// ValidationErrors represents multiple validation errors
type ValidationErrors []ValidationError

func (e ValidationErrors) Error() string {
	messages := make([]string, len(e))
	for i, err := range e {
		messages[i] = err.Error()
	}
	return strings.Join(messages, "; ")
}

// NewRequestValidator creates a new request validator
func NewRequestValidator(config *Config, logger *zap.Logger) (*RequestValidator, error) {
	// Initialize validator
	v := validator.New()

	// Register custom validation functions
	if err := registerCustomValidations(v); err != nil {
		return nil, fmt.Errorf("failed to register custom validations: %w", err)
	}

	// Initialize CEL environment for complex expressions
	env, err := cel.NewEnv(
		cel.Declarations(
			decls.NewVar("request", decls.NewMapType(decls.String, decls.Dyn)),
			decls.NewVar("headers", decls.NewMapType(decls.String, decls.String)),
			decls.NewVar("body", decls.NewMapType(decls.String, decls.Dyn)),
			decls.NewVar("query", decls.NewMapType(decls.String, decls.String)),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize CEL environment: %w", err)
	}

	rv := &RequestValidator{
		config:      config,
		logger:      logger,
		validator:   v,
		celEnv:      env,
		customFuncs: make(map[string]ValidationFunc),
	}

	// Register built-in custom functions
	rv.registerBuiltinFunctions()

	return rv, nil
}

// Validate validates a request against rules
func (rv *RequestValidator) Validate(req *Request, rules ValidationRules) error {
	var errors ValidationErrors

	// Validate content type
	if err := rv.validateContentType(req, rules); err != nil {
		errors = append(errors, err.(ValidationError))
	}

	// Validate payload size
	if err := rv.validatePayloadSize(req, rules); err != nil {
		errors = append(errors, err.(ValidationError))
	}

	// Parse body for field validation
	var bodyData map[string]interface{}
	if len(req.Body) > 0 {
		if err := json.Unmarshal(req.Body, &bodyData); err != nil {
			errors = append(errors, ValidationError{
				Field:   "body",
				Message: "invalid JSON format",
				Code:    "INVALID_JSON",
			})
		} else {
			// Validate required fields
			if err := rv.validateRequiredFields(bodyData, rules); err != nil {
				if ve, ok := err.(ValidationErrors); ok {
					errors = append(errors, ve...)
				}
			}

			// Validate field validators
			if err := rv.validateFields(bodyData, rules); err != nil {
				if ve, ok := err.(ValidationErrors); ok {
					errors = append(errors, ve...)
				}
			}
		}
	} else if len(rules.RequiredFields) > 0 {
		// No body but required fields specified
		errors = append(errors, ValidationError{
			Field:   "body",
			Message: "request body is required",
			Code:    "MISSING_BODY",
		})
	}

	// Run custom validation rules
	if err := rv.runCustomRules(req, bodyData, rules); err != nil {
		if ve, ok := err.(ValidationErrors); ok {
			errors = append(errors, ve...)
		}
	}

	if len(errors) > 0 {
		return errors
	}

	return nil
}

// validateContentType validates the content type
func (rv *RequestValidator) validateContentType(req *Request, rules ValidationRules) error {
	if len(rules.ContentTypes) == 0 {
		return nil // No content type restrictions
	}

	contentType := ""
	if headers, ok := req.Headers["Content-Type"]; ok && len(headers) > 0 {
		contentType = headers[0]
	}

	for _, allowed := range rules.ContentTypes {
		if strings.Contains(contentType, allowed) {
			return nil
		}
	}

	return ValidationError{
		Field:   "Content-Type",
		Value:   contentType,
		Message: fmt.Sprintf("unsupported content type, allowed: %v", rules.ContentTypes),
		Code:    "INVALID_CONTENT_TYPE",
	}
}

// validatePayloadSize validates the payload size
func (rv *RequestValidator) validatePayloadSize(req *Request, rules ValidationRules) error {
	if rules.MaxPayloadSize <= 0 {
		return nil // No size restriction
	}

	size := len(req.Body)
	if int64(size) > rules.MaxPayloadSize {
		return ValidationError{
			Field:   "body",
			Value:   size,
			Message: fmt.Sprintf("payload size %d exceeds maximum %d", size, rules.MaxPayloadSize),
			Code:    "PAYLOAD_TOO_LARGE",
		}
	}

	return nil
}

// validateRequiredFields validates that all required fields are present
func (rv *RequestValidator) validateRequiredFields(data map[string]interface{}, rules ValidationRules) error {
	var errors ValidationErrors

	for _, field := range rules.RequiredFields {
		value, exists := getFieldValue(data, field)
		if !exists || isEmptyValue(value) {
			errors = append(errors, ValidationError{
				Field:   field,
				Message: "field is required",
				Code:    "REQUIRED_FIELD",
			})
		}
	}

	if len(errors) > 0 {
		return errors
	}
	return nil
}

// validateFields validates individual fields
func (rv *RequestValidator) validateFields(data map[string]interface{}, rules ValidationRules) error {
	var errors ValidationErrors

	for fieldPath, validator := range rules.FieldValidators {
		value, exists := getFieldValue(data, fieldPath)
		
		// Check if field is required
		if validator.Required && (!exists || isEmptyValue(value)) {
			errors = append(errors, ValidationError{
				Field:   fieldPath,
				Message: "field is required",
				Code:    "REQUIRED_FIELD",
			})
			continue
		}

		// Skip validation if field doesn't exist and is not required
		if !exists {
			continue
		}

		// Validate field type
		if err := rv.validateFieldType(fieldPath, value, validator); err != nil {
			errors = append(errors, err.(ValidationError))
			continue
		}

		// Apply field-specific validations
		if err := rv.applyFieldValidation(fieldPath, value, validator); err != nil {
			if ve, ok := err.(ValidationErrors); ok {
				errors = append(errors, ve...)
			} else if ve, ok := err.(ValidationError); ok {
				errors = append(errors, ve)
			}
		}
	}

	if len(errors) > 0 {
		return errors
	}
	return nil
}

// validateFieldType validates the type of a field
func (rv *RequestValidator) validateFieldType(field string, value interface{}, validator FieldValidator) error {
	expectedType := validator.Type
	actualType := getValueType(value)

	if !isTypeCompatible(actualType, expectedType) {
		return ValidationError{
			Field:   field,
			Value:   value,
			Message: fmt.Sprintf("expected type %s, got %s", expectedType, actualType),
			Code:    "INVALID_TYPE",
		}
	}

	return nil
}

// applyFieldValidation applies specific validation rules to a field
func (rv *RequestValidator) applyFieldValidation(field string, value interface{}, validator FieldValidator) error {
	var errors ValidationErrors

	switch validator.Type {
	case "string":
		if err := rv.validateString(field, value, validator); err != nil {
			errors = append(errors, err.(ValidationError))
		}

	case "number":
		if err := rv.validateNumber(field, value, validator); err != nil {
			errors = append(errors, err.(ValidationError))
		}

	case "array":
		if err := rv.validateArray(field, value, validator); err != nil {
			errors = append(errors, err.(ValidationError))
		}

	case "object":
		// Nested object validation would go here
		if validator.CustomFunc != "" {
			if err := rv.runCustomFunc(field, value, validator); err != nil {
				errors = append(errors, ValidationError{
					Field:   field,
					Message: err.Error(),
					Code:    "CUSTOM_VALIDATION_FAILED",
				})
			}
		}
	}

	// Check enum values
	if len(validator.Enum) > 0 {
		if err := rv.validateEnum(field, value, validator); err != nil {
			errors = append(errors, err.(ValidationError))
		}
	}

	// Run custom function if specified
	if validator.CustomFunc != "" {
		if err := rv.runCustomFunc(field, value, validator); err != nil {
			errors = append(errors, ValidationError{
				Field:   field,
				Message: err.Error(),
				Code:    "CUSTOM_VALIDATION_FAILED",
			})
		}
	}

	if len(errors) > 0 {
		return errors
	}
	return nil
}

// validateString validates string fields
func (rv *RequestValidator) validateString(field string, value interface{}, validator FieldValidator) error {
	str, ok := value.(string)
	if !ok {
		return ValidationError{
			Field:   field,
			Value:   value,
			Message: "value is not a string",
			Code:    "INVALID_TYPE",
		}
	}

	// Check min length
	if validator.MinLength != nil && len(str) < *validator.MinLength {
		return ValidationError{
			Field:   field,
			Value:   len(str),
			Message: fmt.Sprintf("length must be at least %d", *validator.MinLength),
			Code:    "MIN_LENGTH",
		}
	}

	// Check max length
	if validator.MaxLength != nil && len(str) > *validator.MaxLength {
		return ValidationError{
			Field:   field,
			Value:   len(str),
			Message: fmt.Sprintf("length must not exceed %d", *validator.MaxLength),
			Code:    "MAX_LENGTH",
		}
	}

	// Check pattern
	if validator.Pattern != "" {
		matched, err := regexp.MatchString(validator.Pattern, str)
		if err != nil {
			rv.logger.Error("Invalid regex pattern", zap.String("pattern", validator.Pattern), zap.Error(err))
		} else if !matched {
			return ValidationError{
				Field:   field,
				Message: fmt.Sprintf("value does not match pattern %s", validator.Pattern),
				Code:    "PATTERN_MISMATCH",
			}
		}
	}

	return nil
}

// validateNumber validates numeric fields
func (rv *RequestValidator) validateNumber(field string, value interface{}, validator FieldValidator) error {
	num := toFloat64(value)
	if num == nil {
		return ValidationError{
			Field:   field,
			Value:   value,
			Message: "value is not a number",
			Code:    "INVALID_TYPE",
		}
	}

	// Check min value
	if validator.Min != nil && *num < *validator.Min {
		return ValidationError{
			Field:   field,
			Value:   *num,
			Message: fmt.Sprintf("value must be at least %f", *validator.Min),
			Code:    "MIN_VALUE",
		}
	}

	// Check max value
	if validator.Max != nil && *num > *validator.Max {
		return ValidationError{
			Field:   field,
			Value:   *num,
			Message: fmt.Sprintf("value must not exceed %f", *validator.Max),
			Code:    "MAX_VALUE",
		}
	}

	return nil
}

// validateArray validates array fields
func (rv *RequestValidator) validateArray(field string, value interface{}, validator FieldValidator) error {
	arr, ok := value.([]interface{})
	if !ok {
		return ValidationError{
			Field:   field,
			Value:   value,
			Message: "value is not an array",
			Code:    "INVALID_TYPE",
		}
	}

	// Check min length
	if validator.MinLength != nil && len(arr) < *validator.MinLength {
		return ValidationError{
			Field:   field,
			Value:   len(arr),
			Message: fmt.Sprintf("array must have at least %d items", *validator.MinLength),
			Code:    "MIN_ITEMS",
		}
	}

	// Check max length
	if validator.MaxLength != nil && len(arr) > *validator.MaxLength {
		return ValidationError{
			Field:   field,
			Value:   len(arr),
			Message: fmt.Sprintf("array must not exceed %d items", *validator.MaxLength),
			Code:    "MAX_ITEMS",
		}
	}

	return nil
}

// validateEnum validates enum values
func (rv *RequestValidator) validateEnum(field string, value interface{}, validator FieldValidator) error {
	strValue := fmt.Sprintf("%v", value)
	
	for _, allowed := range validator.Enum {
		if strValue == allowed {
			return nil
		}
	}

	return ValidationError{
		Field:   field,
		Value:   value,
		Message: fmt.Sprintf("value must be one of %v", validator.Enum),
		Code:    "INVALID_ENUM",
	}
}

// runCustomRules runs custom validation rules
func (rv *RequestValidator) runCustomRules(req *Request, bodyData map[string]interface{}, rules ValidationRules) error {
	var errors ValidationErrors

	for _, rule := range rules.CustomRules {
		if err := rv.evaluateCustomRule(req, bodyData, rule); err != nil {
			errors = append(errors, ValidationError{
				Field:   rule.Name,
				Message: rule.ErrorMessage,
				Code:    "CUSTOM_RULE_FAILED",
			})
		}
	}

	if len(errors) > 0 {
		return errors
	}
	return nil
}

// evaluateCustomRule evaluates a custom validation rule
func (rv *RequestValidator) evaluateCustomRule(req *Request, bodyData map[string]interface{}, rule CustomValidationRule) error {
	// Parse and compile the CEL expression
	ast, issues := rv.celEnv.Compile(rule.Expression)
	if issues != nil && issues.Err() != nil {
		rv.logger.Error("Failed to compile CEL expression",
			zap.String("expression", rule.Expression),
			zap.Error(issues.Err()),
		)
		return fmt.Errorf("invalid expression: %w", issues.Err())
	}

	// Create the CEL program
	prg, err := rv.celEnv.Program(ast)
	if err != nil {
		return fmt.Errorf("failed to create program: %w", err)
	}

	// Prepare input data
	input := map[string]interface{}{
		"request": map[string]interface{}{
			"method": req.Method,
			"path":   req.Path,
		},
		"headers": req.Headers,
		"body":    bodyData,
		"query":   req.QueryParams,
	}

	// Evaluate the expression
	out, _, err := prg.Eval(input)
	if err != nil {
		rv.logger.Error("Failed to evaluate CEL expression",
			zap.String("expression", rule.Expression),
			zap.Error(err),
		)
		return fmt.Errorf("evaluation failed: %w", err)
	}

	// Check if the result is true
	result, ok := out.Value().(bool)
	if !ok {
		return fmt.Errorf("expression did not return a boolean")
	}

	if !result {
		return fmt.Errorf("custom validation failed")
	}

	return nil
}

// runCustomFunc runs a custom validation function
func (rv *RequestValidator) runCustomFunc(field string, value interface{}, validator FieldValidator) error {
	if fn, exists := rv.customFuncs[validator.CustomFunc]; exists {
		return fn(value, validator)
	}
	return fmt.Errorf("custom function %s not found", validator.CustomFunc)
}

// registerBuiltinFunctions registers built-in custom validation functions
func (rv *RequestValidator) registerBuiltinFunctions() {
	// Email validation
	rv.customFuncs["email"] = func(value interface{}, rule FieldValidator) error {
		str, ok := value.(string)
		if !ok {
			return fmt.Errorf("value is not a string")
		}
		emailRegex := `^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`
		if matched, _ := regexp.MatchString(emailRegex, str); !matched {
			return fmt.Errorf("invalid email format")
		}
		return nil
	}

	// URL validation
	rv.customFuncs["url"] = func(value interface{}, rule FieldValidator) error {
		str, ok := value.(string)
		if !ok {
			return fmt.Errorf("value is not a string")
		}
		urlRegex := `^(https?|ftp)://[^\s/$.?#].[^\s]*$`
		if matched, _ := regexp.MatchString(urlRegex, str); !matched {
			return fmt.Errorf("invalid URL format")
		}
		return nil
	}

	// Phone validation
	rv.customFuncs["phone"] = func(value interface{}, rule FieldValidator) error {
		str, ok := value.(string)
		if !ok {
			return fmt.Errorf("value is not a string")
		}
		phoneRegex := `^\+?[1-9]\d{1,14}$` // E.164 format
		if matched, _ := regexp.MatchString(phoneRegex, str); !matched {
			return fmt.Errorf("invalid phone format")
		}
		return nil
	}

	// UUID validation
	rv.customFuncs["uuid"] = func(value interface{}, rule FieldValidator) error {
		str, ok := value.(string)
		if !ok {
			return fmt.Errorf("value is not a string")
		}
		uuidRegex := `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`
		if matched, _ := regexp.MatchString(uuidRegex, str); !matched {
			return fmt.Errorf("invalid UUID format")
		}
		return nil
	}

	// Date validation (ISO 8601)
	rv.customFuncs["date"] = func(value interface{}, rule FieldValidator) error {
		str, ok := value.(string)
		if !ok {
			return fmt.Errorf("value is not a string")
		}
		dateRegex := `^\d{4}-\d{2}-\d{2}$`
		if matched, _ := regexp.MatchString(dateRegex, str); !matched {
			return fmt.Errorf("invalid date format (expected YYYY-MM-DD)")
		}
		return nil
	}

	// Datetime validation (ISO 8601)
	rv.customFuncs["datetime"] = func(value interface{}, rule FieldValidator) error {
		str, ok := value.(string)
		if !ok {
			return fmt.Errorf("value is not a string")
		}
		datetimeRegex := `^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(.\d{3})?Z?$`
		if matched, _ := regexp.MatchString(datetimeRegex, str); !matched {
			return fmt.Errorf("invalid datetime format (expected ISO 8601)")
		}
		return nil
	}
}

// RegisterCustomFunc registers a custom validation function
func (rv *RequestValidator) RegisterCustomFunc(name string, fn ValidationFunc) {
	rv.customFuncs[name] = fn
}

// Helper functions

// getFieldValue retrieves a field value from nested data using dot notation
func getFieldValue(data map[string]interface{}, fieldPath string) (interface{}, bool) {
	parts := strings.Split(fieldPath, ".")
	current := data

	for i, part := range parts {
		if i == len(parts)-1 {
			// Last part - return the value
			value, exists := current[part]
			return value, exists
		}

		// Navigate deeper
		next, exists := current[part]
		if !exists {
			return nil, false
		}

		// Check if next is a map
		nextMap, ok := next.(map[string]interface{})
		if !ok {
			return nil, false
		}

		current = nextMap
	}

	return nil, false
}

// isEmptyValue checks if a value is empty
func isEmptyValue(value interface{}) bool {
	if value == nil {
		return true
	}

	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.String, reflect.Array, reflect.Slice, reflect.Map:
		return v.Len() == 0
	case reflect.Bool:
		return !v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return v.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return v.Float() == 0
	case reflect.Ptr, reflect.Interface:
		return v.IsNil()
	}

	return false
}

// getValueType returns the type of a value
func getValueType(value interface{}) string {
	if value == nil {
		return "null"
	}

	switch value.(type) {
	case string:
		return "string"
	case float64, float32, int, int32, int64:
		return "number"
	case bool:
		return "boolean"
	case []interface{}:
		return "array"
	case map[string]interface{}:
		return "object"
	default:
		return fmt.Sprintf("%T", value)
	}
}

// isTypeCompatible checks if actual type is compatible with expected type
func isTypeCompatible(actual, expected string) bool {
	if actual == expected {
		return true
	}

	// Special cases
	if expected == "number" && (actual == "float64" || actual == "float32" || actual == "int" || actual == "int32" || actual == "int64") {
		return true
	}

	return false
}

// toFloat64 converts a value to float64
func toFloat64(value interface{}) *float64 {
	switch v := value.(type) {
	case float64:
		return &v
	case float32:
		f := float64(v)
		return &f
	case int:
		f := float64(v)
		return &f
	case int32:
		f := float64(v)
		return &f
	case int64:
		f := float64(v)
		return &f
	default:
		return nil
	}
}

// registerCustomValidations registers custom validation tags for the validator
func registerCustomValidations(v *validator.Validate) error {
	// Add custom validation tags here
	return nil
}
