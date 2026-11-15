// Package user contains the schema-bearing Go types for the "user" secret
// type, referenced by Instance.spec.userSecretRef.
//
// +k8s:openapi-gen=true
package user

// UserSecretData holds the initial credentials of the OpenSearch admin user.
// They are copied when the instance is created; later edits to the Secret do
// not change the password.
type UserSecretData struct {
	// Username must be "admin", the user the operator manages. Optional.
	// +optional
	Username string `json:"username,omitempty"`
	// Password of the admin user.
	Password string `json:"password"`
}
