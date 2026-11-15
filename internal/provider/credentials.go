package provider

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

// adminUsername is the only user the operator seeds; it reads just the
// password from the admin credentials Secret.
const adminUsername = "admin"

var userSecretRefPath = field.NewPath("spec", "userSecretRef")

// adminCredentialsName names the provider's copy of the credentials in
// spec.userSecretRef. The copy is taken once, so editing the user's Secret
// later does not change the admin password.
func adminCredentialsName(clusterName string) string {
	return clusterName + "-admin-credentials"
}

// adminCredentialsSecretName is the Secret with the admin user's credentials:
// the copy of the user's, or the one the operator generated.
func adminCredentialsSecretName(in *corev1alpha1.Instance) string {
	if in.Spec.UserSecretRef != nil {
		return adminCredentialsName(in.Name)
	}
	return adminSecretName(in.Name)
}

// validateUserSecretRef rejects adding spec.userSecretRef to an existing
// Instance; core only keeps it from changing once set.
func validateUserSecretRef(in, old *corev1alpha1.Instance) field.ErrorList {
	if old != nil && old.Spec.UserSecretRef == nil && in.Spec.UserSecretRef != nil {
		return field.ErrorList{field.Forbidden(userSecretRefPath, "can only be set when the instance is created")}
	}
	return nil
}

// validateUserSecret checks the user's credentials until they are copied.
func validateUserSecret(c *controller.Context) (field.ErrorList, error) {
	ref := c.Instance().Spec.UserSecretRef
	if ref == nil {
		return nil, nil
	}
	copied, err := c.Exists(&corev1.Secret{}, adminCredentialsName(c.Name()))
	if err != nil || copied {
		return nil, err
	}
	path := userSecretRefPath.Child("name")
	secret := &corev1.Secret{}
	if err := c.Get(secret, ref.Name); err != nil {
		if apierrors.IsNotFound(err) {
			return field.ErrorList{field.NotFound(path, ref.Name)}, nil
		}
		return nil, fmt.Errorf("get user secret %q: %w", ref.Name, err)
	}
	if err := checkUserSecret(secret); err != "" {
		return field.ErrorList{field.Invalid(path, ref.Name, err)}, nil
	}
	return nil, nil
}

func checkUserSecret(secret *corev1.Secret) string {
	if len(secret.Data["password"]) == 0 {
		return `missing "password"`
	}
	if u := string(secret.Data["username"]); u != "" && u != adminUsername {
		return fmt.Sprintf("username must be %q, the user the operator manages, or left out", adminUsername)
	}
	return ""
}

// ensureAdminCredentials copies the user's credentials once and returns the
// copy's name, or "" without spec.userSecretRef.
func ensureAdminCredentials(c *controller.Context) (string, error) {
	ref := c.Instance().Spec.UserSecretRef
	if ref == nil {
		return "", nil
	}
	name := adminCredentialsName(c.Name())
	exists, err := c.Exists(&corev1.Secret{}, name)
	if err != nil || exists {
		return name, err
	}
	source := &corev1.Secret{}
	if err := c.Get(source, ref.Name); err != nil {
		return "", fmt.Errorf("get user secret %q: %w", ref.Name, err)
	}
	if msg := checkUserSecret(source); msg != "" {
		return "", fmt.Errorf("user secret %q: %s", ref.Name, msg)
	}
	creds := &corev1.Secret{
		ObjectMeta: c.ObjectMeta(name),
		Data: map[string][]byte{
			"username": []byte(adminUsername),
			"password": source.Data["password"],
		},
	}
	if err := c.Apply(creds); err != nil {
		return "", fmt.Errorf("apply admin credentials secret: %w", err)
	}
	return name, nil
}
