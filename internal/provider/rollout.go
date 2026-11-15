package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/log"

	opensearchv1 "github.com/opensearch-project/opensearch-k8s-operator/opensearch-operator/api/opensearch.org/v1"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

// Record keys of the rollout state Secret. Bump the inputs key's version
// whenever inputsHash changes what it fingerprints, so records of the earlier
// format are recognised (and their changes held) instead of every Instance
// looking edited after a provider upgrade.
const (
	rolloutSpecKey   = "cluster.spec"
	rolloutInputsKey = "cluster.inputs-v1"
)

// rolloutStateName is the provider-owned Secret recording what was last
// applied to an Instance's OpenSearchCluster.
func rolloutStateName(instanceName string) string {
	return instanceName + "-rollout-state"
}

// applyClusterGated applies the rendered OpenSearchCluster when allowed. Most changes to its spec roll the OpenSearch pods.
// A change that follows from the user editing the Instance is applied right
// away. A change the provider makes on its own, with the Instance unchanged
// (new rendering defaults or a new default version after a provider upgrade),
// is a maintenance action: it is held until the Instance's maintenance
// tolerance or an approval allows it.
//
// What was last applied, and from which inputs, is recorded in a
// provider-owned Secret. A cluster without a record was last applied by a
// provider predating this gate: it is applied and recorded as the baseline.
//
// userInputs carries the rendering inputs that come from user-owned objects
// other than the Instance (the backup storages), so editing them counts as a
// user change too.
func applyClusterGated(c *controller.Context, cluster *opensearchv1.OpenSearchCluster, exists bool, userInputs any) error {
	record := map[string]string{}
	state := &corev1.Secret{}
	if err := c.Get(state, rolloutStateName(c.Name())); err == nil {
		for _, k := range []string{rolloutSpecKey, rolloutInputsKey} {
			if v, ok := state.Data[k]; ok {
				record[k] = string(v)
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("read rollout state: %w", err)
	}

	spec, err := hashJSON(cluster.Spec)
	if err != nil {
		return err
	}
	inputs, err := inputsHash(c, userInputs)
	if err != nil {
		return err
	}

	recordedSpec := record[rolloutSpecKey]
	recordedInputs := record[rolloutInputsKey]
	unchanged := recordedSpec == spec
	// Without recorded inputs (a record of an earlier format) a changed
	// rendering cannot be attributed to the user, so it is held.
	userChange := recordedInputs != "" && recordedInputs != inputs
	if exists && recordedSpec != "" && !unchanged && !userChange {
		token, err := rolloutToken(c, spec)
		if err != nil {
			return err
		}
		if !c.RequestMaintenance(token,
			"Apply the provider's updated OpenSearch cluster configuration (rolling restart)",
			controller.MaintenanceRollingRestart) {
			log.FromContext(c.Context()).Info("Holding provider-initiated change until maintenance is approved",
				"cluster", cluster.Name, "token", token)
			return nil
		}
	}

	if err := c.Apply(cluster); err != nil {
		return err
	}
	if unchanged && recordedInputs == inputs {
		return nil
	}
	saved := &corev1.Secret{
		ObjectMeta: c.ObjectMeta(rolloutStateName(c.Name())),
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			rolloutSpecKey:   []byte(spec),
			rolloutInputsKey: []byte(inputs),
		},
	}
	if err := c.Apply(saved); err != nil {
		return fmt.Errorf("record rollout state: %w", err)
	}
	return nil
}

// rolloutToken names a held change: the provider release that makes it and
// the rendering it leads to, so it is stable while pending and a later
// release asks for a new approval.
func rolloutToken(c *controller.Context, spec string) (string, error) {
	spec8 := spec[:8]
	ps, err := c.ProviderSpec()
	if err != nil {
		// The token must stay stable while the action is pending; a transient
		// Provider read failure must not flap it to the fallback.
		return "", fmt.Errorf("read provider spec for the approval token: %w", err)
	}
	if ps.Release != nil && ps.Release.Version != "" {
		return fmt.Sprintf("provider-%s-%s", ps.Release.Version, spec8), nil
	}
	return "provider-change-" + spec8, nil
}

// inputsHash fingerprints the user inputs the OpenSearchCluster is rendered
// from. They are taken from the stored Instance, not from the copy Sync is
// handed: the runtime fills that copy's component versions in from the
// provider's defaults, so a provider release moving a default would
// otherwise look like a user edit. Maintenance approvals, backup schedules,
// monitoring and the deletion policy are left out: changing them is not a
// change to the cluster.
func inputsHash(c *controller.Context, userInputs any) (string, error) {
	stored := &corev1alpha1.Instance{}
	if err := c.Get(stored, c.Name()); err != nil {
		return "", fmt.Errorf("read stored Instance: %w", err)
	}
	s := stored.Spec
	return hashJSON(struct {
		Version       string `json:"version,omitempty"`
		Topology      any    `json:"topology,omitempty"`
		Parameters    any    `json:"parameters,omitempty"`
		Components    any    `json:"components,omitempty"`
		UserSecretRef any    `json:"userSecretRef,omitempty"`
		Other         any    `json:"other,omitempty"`
	}{s.Version, s.Topology, s.Parameters, s.Components, s.UserSecretRef, userInputs})
}

func hashJSON(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
