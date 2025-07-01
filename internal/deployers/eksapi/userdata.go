package eksapi

import (
	"bytes"
	"fmt"
	"text/template"

	"github.com/aws/aws-k8s-tester/internal/deployers/eksapi/templates"
)

func generateUserData(format string, cluster *Cluster, opts *deployerOptions) (string, bool, error) {
	userDataIsMimePart := true
	var t *template.Template
	switch format {
	case "bootstrap.sh":
		t = templates.UserDataBootstrapSh
	case "nodeadm":
		// TODO: replace the YAML template with proper usage of the nodeadm API go types
		t = templates.UserDataNodeadm
	case "bottlerocket":
		t = templates.UserDataBottlerocket
		userDataIsMimePart = false
	case "soci":
		t = templates.UserDataSoci
	default:
		return "", false, fmt.Errorf("uknown user data format: '%s'", format)
	}

	kubeletFeatureGates := map[string]bool{}
	// DRA is in beta for 1.33, and so needs to be explicitly enabled.
	if opts.KubernetesVersion == "1.33" {
		kubeletFeatureGates["DynamicResourceAllocation"] = true
	}

	var buf bytes.Buffer
	templateData := templates.UserDataTemplateData{
		APIServerEndpoint:    cluster.endpoint,
		CertificateAuthority: cluster.certificateAuthorityData,
		CIDR:                 cluster.cidr,
		Name:                 cluster.name,
		KubeletFeatureGates:  kubeletFeatureGates,
		ClusterName:          cluster.name,
		SociEnabled:          opts.SociEnabled,
	}

	// Set SOCI-specific default values when the feature is enabled
	if format == "soci" || opts.SociEnabled {
		// Default values for SOCI configuration
		templateData.MaxConcurrentDownloads = "-1"        // default value
		templateData.MaxConcurrentDownloadsPerImage = "8" // default value
		templateData.MaxConcurrentUnpacksPerImage = "4"   // default value
	}

	if err := t.Execute(&buf, templateData); err != nil {
		return "", false, err
	}
	return buf.String(), userDataIsMimePart, nil
}
