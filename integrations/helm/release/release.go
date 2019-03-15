package release

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"os/exec"
	"time"

	"github.com/ghodss/yaml"
	"github.com/go-kit/kit/log"
	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
	"k8s.io/helm/pkg/chartutil"
	"k8s.io/helm/pkg/getter"
	k8shelm "k8s.io/helm/pkg/helm"
	helmenv "k8s.io/helm/pkg/helm/environment"
	hapi_release "k8s.io/helm/pkg/proto/hapi/release"

	"github.com/weaveworks/flux"
	fluxk8s "github.com/weaveworks/flux/cluster/kubernetes"
	flux_v1beta1 "github.com/weaveworks/flux/integrations/apis/flux.weave.works/v1beta1"
	helmutil "k8s.io/helm/pkg/releaseutil"
)

type Action string

const (
	InstallAction Action = "CREATE"
	UpgradeAction Action = "UPDATE"
)

// Release contains clients needed to provide functionality related to helm releases
type Release struct {
	logger     log.Logger
	HelmClient *k8shelm.Client
}

type Releaser interface {
	GetDeployedRelease(name string) (*hapi_release.Release, error)
	Install(dir string, releaseName string, fhr flux_v1beta1.HelmRelease, action Action, opts InstallOptions) (*hapi_release.Release, error)
}

type DeployInfo struct {
	Name string
}

type InstallOptions struct {
	DryRun    bool
	ReuseName bool
}

// New creates a new Release instance.
func New(logger log.Logger, helmClient *k8shelm.Client) *Release {
	r := &Release{
		logger:     logger,
		HelmClient: helmClient,
	}
	return r
}

// GetReleaseName either retrieves the release name from the Custom Resource or constructs a new one
// in the form : $Namespace-$CustomResourceName
func GetReleaseName(fhr flux_v1beta1.HelmRelease) string {
	namespace := fhr.Namespace
	if namespace == "" {
		namespace = "default"
	}
	releaseName := fhr.Spec.ReleaseName
	if releaseName == "" {
		releaseName = fmt.Sprintf("%s-%s", namespace, fhr.Name)
	}

	return releaseName
}

// GetDeployedRelease returns a release with Deployed status
func (r *Release) GetDeployedRelease(name string) (*hapi_release.Release, error) {
	rls, err := r.HelmClient.ReleaseContent(name)
	if err != nil {
		return nil, err
	}
	if rls.Release.Info.Status.GetCode() == hapi_release.Status_DEPLOYED {
		return rls.GetRelease(), nil
	}
	return nil, nil
}

func (r *Release) canDelete(name string) (bool, error) {
	rls, err := r.HelmClient.ReleaseStatus(name)

	if err != nil {
		r.logger.Log("error", fmt.Sprintf("Error finding status for release (%s): %#v", name, err))
		return false, err
	}
	/*
		"UNKNOWN":          0,
		"DEPLOYED":         1,
		"DELETED":          2,
		"SUPERSEDED":       3,
		"FAILED":           4,
		"DELETING":         5,
		"PENDING_INSTALL":  6,
		"PENDING_UPGRADE":  7,
		"PENDING_ROLLBACK": 8,
	*/
	status := rls.GetInfo().GetStatus()
	switch status.Code {
	case 1, 4:
		r.logger.Log("info", fmt.Sprintf("Deleting release %s", name))
		return true, nil
	case 2:
		r.logger.Log("info", fmt.Sprintf("Release %s already deleted", name))
		return false, nil
	default:
		r.logger.Log("info", fmt.Sprintf("Release %s with status %s cannot be deleted", name, status.Code.String()))
		return false, fmt.Errorf("release %s with status %s cannot be deleted", name, status.Code.String())
	}
}

// Install performs a Chart release given the directory containing the
// charts, and the HelmRelease specifying the release. Depending
// on the release type, this is either a new release, or an upgrade of
// an existing one.
//
// TODO(michael): cloneDir is only relevant if installing from git;
// either split this procedure into two varieties, or make it more
// general and calculate the path to the chart in the caller.
func (r *Release) Install(chartPath, releaseName string, fhr flux_v1beta1.HelmRelease, action Action, opts InstallOptions, kubeClient *kubernetes.Clientset) (*hapi_release.Release, error) {
	if chartPath == "" {
		return nil, fmt.Errorf("empty path to chart supplied for resource %q", fhr.ResourceID().String())
	}
	_, err := os.Stat(chartPath)
	switch {
	case os.IsNotExist(err):
		return nil, fmt.Errorf("no file or dir at path to chart: %s", chartPath)
	case err != nil:
		return nil, fmt.Errorf("error statting path given for chart %s: %s", chartPath, err.Error())
	}

	r.logger.Log("info", fmt.Sprintf("processing release %s (as %s)", fhr.Spec.ReleaseName, releaseName),
		"action", fmt.Sprintf("%v", action),
		"options", fmt.Sprintf("%+v", opts),
		"timeout", fmt.Sprintf("%vs", fhr.GetTimeout()))

	// Read values from given valueFile paths (configmaps, etc.)
	mergedValues := chartutil.Values{}
	for _, valueFile := range fhr.Spec.ValueFiles {
		// Read the contents of the file
		b, err := readFile(valueFile)
		if err != nil {
			r.logger.Log("error", fmt.Sprintf("Cannot read value file [%s] for Chart release [%s]: %#v", valueFile, fhr.Spec.ReleaseName, err))
			return nil, err
		}

		// Load into values and merge
		var values chartutil.Values
		err = yaml.Unmarshal(b, &values)
		if err != nil {
			r.logger.Log("error", fmt.Sprintf("Cannot yaml.Unmarshal value file [%s] for Chart release [%s]: %#v", valueFile, fhr.Spec.ReleaseName, err))
			return nil, err
		}
		mergedValues = mergeValues(mergedValues, values)
	}
	for _, valueFileSecret := range fhr.Spec.ValueFileSecrets {
		// Read the contents of the secret
		secret, err := kubeClient.CoreV1().Secrets(fhr.Namespace).Get(valueFileSecret.Name, v1.GetOptions{})
		if err != nil {
			r.logger.Log("error", fmt.Sprintf("Cannot get secret [%s] for Chart release [%s]: %#v", valueFileSecret.Name, fhr.Spec.ReleaseName, err))
			return nil, err
		}

		// Load values.yaml file and merge
		var values chartutil.Values
		err = yaml.Unmarshal(secret.Data["values.yaml"], &values)
		if err != nil {
			r.logger.Log("error", fmt.Sprintf("Cannot yaml.Unmarshal values.yaml in secret [%s] for Chart release [%s]: %#v", valueFileSecret.Name, fhr.Spec.ReleaseName, err))
			return nil, err
		}
		mergedValues = mergeValues(mergedValues, values)
	}
	// Merge in values after valueFiles
	mergedValues = mergeValues(mergedValues, fhr.Spec.Values)

	strVals, err := mergedValues.YAML()
	if err != nil {
		r.logger.Log("error", fmt.Sprintf("Problem with supplied customizations for Chart release [%s]: %#v", fhr.Spec.ReleaseName, err))
		return nil, err
	}
	rawVals := []byte(strVals)

	switch action {
	case InstallAction:
		res, err := r.HelmClient.InstallRelease(
			chartPath,
			fhr.GetNamespace(),
			k8shelm.ValueOverrides(rawVals),
			k8shelm.ReleaseName(releaseName),
			k8shelm.InstallDryRun(opts.DryRun),
			k8shelm.InstallReuseName(opts.ReuseName),
			k8shelm.InstallTimeout(fhr.GetTimeout()),
		)

		if err != nil {
			r.logger.Log("error", fmt.Sprintf("Chart release failed: %s: %#v", fhr.Spec.ReleaseName, err))
			// purge the release if the install failed but only if this is the first revision
			history, err := r.HelmClient.ReleaseHistory(releaseName, k8shelm.WithMaxHistory(2))
			if err == nil && len(history.Releases) == 1 && history.Releases[0].Info.Status.Code == hapi_release.Status_FAILED {
				r.logger.Log("info", fmt.Sprintf("Deleting failed release: [%s]", fhr.Spec.ReleaseName))
				_, err = r.HelmClient.DeleteRelease(releaseName, k8shelm.DeletePurge(true))
				if err != nil {
					r.logger.Log("error", fmt.Sprintf("Release deletion error: %#v", err))
					return nil, err
				}
			}
			return nil, err
		}
		if !opts.DryRun {
			r.annotateResources(res.Release, fhr)
		}
		return res.Release, err
	case UpgradeAction:
		res, err := r.HelmClient.UpdateRelease(
			releaseName,
			chartPath,
			k8shelm.UpdateValueOverrides(rawVals),
			k8shelm.UpgradeDryRun(opts.DryRun),
			k8shelm.UpgradeTimeout(fhr.GetTimeout()),
			k8shelm.ResetValues(fhr.Spec.ResetValues),
			k8shelm.UpgradeForce(fhr.Spec.ForceUpgrade),
		)

		if err != nil {
			r.logger.Log("error", fmt.Sprintf("Chart upgrade release failed: %s: %#v", fhr.Spec.ReleaseName, err))
			return nil, err
		}
		if !opts.DryRun {
			r.annotateResources(res.Release, fhr)
		}
		return res.Release, err
	default:
		err = fmt.Errorf("Valid install options: CREATE, UPDATE. Provided: %s", action)
		r.logger.Log("error", err.Error())
		return nil, err
	}
}

// Delete purges a Chart release
func (r *Release) Delete(name string) error {
	ok, err := r.canDelete(name)
	if !ok {
		if err != nil {
			return err
		}
		return nil
	}

	_, err = r.HelmClient.DeleteRelease(name, k8shelm.DeletePurge(true))
	if err != nil {
		r.logger.Log("error", fmt.Sprintf("Release deletion error: %#v", err))
		return err
	}
	r.logger.Log("info", fmt.Sprintf("Release deleted: [%s]", name))
	return nil
}

// annotateResources annotates each of the resources created (or updated)
// by the release so that we can spot them.
func (r *Release) annotateResources(release *hapi_release.Release, fhr flux_v1beta1.HelmRelease) {
	objs := releaseManifestToUnstructured(release.Manifest, r.logger)
	for namespace, res := range namespacedResourceMap(objs, release.Namespace) {
		args := []string{"annotate", "--overwrite"}
		args = append(args, "--namespace", namespace)
		args = append(args, res...)
		args = append(args, fluxk8s.AntecedentAnnotation+"="+fhrResourceID(fhr).String())

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		cmd := exec.CommandContext(ctx, "kubectl", args...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			r.logger.Log("output", string(output), "err", err)
		}
	}
}

// fhrResourceID constructs a flux.ResourceID for a HelmRelease resource.
func fhrResourceID(fhr flux_v1beta1.HelmRelease) flux.ResourceID {
	return flux.MakeResourceID(fhr.Namespace, "HelmRelease", fhr.Name)
}

// Merges source and destination `chartutils.Values`, preferring values from the source Values
// This is slightly adapted from https://github.com/helm/helm/blob/2332b480c9cb70a0d8a85247992d6155fbe82416/cmd/helm/install.go#L359
func mergeValues(dest, src chartutil.Values) chartutil.Values {
	for k, v := range src {
		// If the key doesn't exist already, then just set the key to that value
		if _, exists := dest[k]; !exists {
			dest[k] = v
			continue
		}
		nextMap, ok := v.(map[string]interface{})
		// If it isn't another map, overwrite the value
		if !ok {
			dest[k] = v
			continue
		}
		// Edge case: If the key exists in the destination, but isn't a map
		destMap, isMap := dest[k].(map[string]interface{})
		// If the source map has a map for this key, prefer it
		if !isMap {
			dest[k] = v
			continue
		}
		// If we got to this point, it is a map in both, so merge them
		dest[k] = mergeValues(destMap, nextMap)
	}
	return dest
}

// readFile loads a file from a local directory or url.
// This is slightly adapted from https://github.com/helm/helm/blob/2332b480c9cb70a0d8a85247992d6155fbe82416/cmd/helm/install.go#L552
func readFile(filePath string) ([]byte, error) {
	var settings helmenv.EnvSettings
	flags := pflag.NewFlagSet("helm-env", pflag.ContinueOnError)
	settings.AddFlags(flags)
	settings.Init(flags)

	u, _ := url.Parse(filePath)
	p := getter.All(settings)

	getterConstructor, err := p.ByScheme(u.Scheme)

	if err != nil {
		return ioutil.ReadFile(filePath)
	}

	getter, err := getterConstructor(filePath, "", "", "")
	if err != nil {
		return []byte{}, err
	}
	data, err := getter.Get(filePath)
	return data.Bytes(), err
}

// releaseManifestToUnstructured turns a string containing YAML
// manifests into an array of Unstructured objects.
func releaseManifestToUnstructured(manifest string, logger log.Logger) []unstructured.Unstructured {
	manifests := helmutil.SplitManifests(manifest)
	var objs []unstructured.Unstructured
	for _, manifest := range manifests {
		bytes, err := yaml.YAMLToJSON([]byte(manifest))
		if err != nil {
			logger.Log("err", err)
			continue
		}

		var u unstructured.Unstructured
		if err := u.UnmarshalJSON(bytes); err != nil {
			logger.Log("err", err)
			continue
		}

		// Helm charts may include list kinds, we are only interested in
		// the items on those lists.
		if u.IsList() {
			l, err := u.ToList()
			if err != nil {
				logger.Log("err", err)
				continue
			}
			objs = append(objs, l.Items...)
			continue
		}

		objs = append(objs, u)
	}
	return objs
}

// namespacedResourceMap iterates over the given objects and maps the
// resource identifier against the namespace from the object, if no
// namespace is present (either because the object kind has no namespace
// or it belongs to the release namespace) it gets mapped against the
// given release namespace.
func namespacedResourceMap(objs []unstructured.Unstructured, releaseNamespace string) map[string][]string {
	resources := make(map[string][]string)
	for _, obj := range objs {
		namespace := obj.GetNamespace()
		if namespace == "" {
			namespace = releaseNamespace
		}
		resource := obj.GetKind() + "/" + obj.GetName()
		resources[namespace] = append(resources[namespace], resource)
	}
	return resources
}
