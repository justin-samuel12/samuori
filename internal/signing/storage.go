package signing

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Storage records where a publication's signatures and attestations were written (OD-25).
const (
	StorageTag       = "cosign-tag"
	StorageReferrers = "oci-referrers+cosign-tag"
)

// Descriptor identifies the image a referrer points to.
type Descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

const emptyConfigType = "application/vnd.oci.empty.v1+json"

var emptyConfig = []byte("{}")

// Referrer builds an OCI 1.1 manifest that carries one layer and names subject, so the registry lists it as a
// referrer of the image. Its artifact type is the layer's media type.
func Referrer(l Layer, subject Descriptor) (Artifact, error) {
	manifest, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"artifactType":  l.MediaType,
		"config":        map[string]any{"mediaType": emptyConfigType, "digest": digest(emptyConfig), "size": len(emptyConfig)},
		"layers":        layerDescriptors([]Layer{l}),
		"subject":       subject,
	})
	if err != nil {
		return Artifact{}, err
	}
	return Artifact{Config: emptyConfig, Manifest: manifest, Layers: []Layer{l}}, nil
}

// Combine builds the Cosign tag image from several layers, dropping duplicates, so new signatures are added to the
// ones already stored instead of replacing them.
func Combine(layers ...Layer) (Artifact, error) {
	seen := map[string]bool{}
	var out []Layer
	for _, l := range layers {
		k := l.MediaType + "\x00" + digest(l.Data) + "\x00" + l.Annotations[SignatureAnnotation]
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, l)
	}
	if len(out) == 0 {
		return Artifact{}, errors.New("signing: nothing to store")
	}
	return artifact(out)
}

// ReadLayers returns the signature and attestation layers of a stored manifest, checking each against its digest.
func ReadLayers(manifestBytes []byte, fetch func(digest string) ([]byte, error)) ([]Layer, error) {
	var m manifest
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		return nil, err
	}
	var out []Layer
	for _, l := range m.Layers {
		if l.MediaType != SimpleSigningType && l.MediaType != DSSEType {
			continue
		}
		data, err := fetch(l.Digest)
		if err != nil {
			return nil, err
		}
		if digest(data) != l.Digest {
			return nil, fmt.Errorf("signing: layer %s does not match its digest", l.Digest)
		}
		out = append(out, Layer{MediaType: l.MediaType, Data: data, Annotations: l.Annotations})
	}
	return out, nil
}

// ArtifactType returns a manifest's artifact type, or "" for an ordinary image.
func ArtifactType(manifestBytes []byte) string {
	var m struct {
		ArtifactType string `json:"artifactType"`
	}
	_ = json.Unmarshal(manifestBytes, &m)
	return m.ArtifactType
}

// Covers reports whether a layer signs or attests imageDigest. It does not check the signature.
func Covers(l Layer, imageDigest string) bool {
	switch l.MediaType {
	case SimpleSigningType:
		var p simpleSigning
		return json.Unmarshal(l.Data, &p) == nil && p.Critical.Image.DockerManifestDigest == imageDigest
	case DSSEType:
		var env Envelope
		if json.Unmarshal(l.Data, &env) != nil {
			return false
		}
		payload, err := base64.StdEncoding.DecodeString(env.Payload)
		if err != nil {
			return false
		}
		var st struct {
			Subject []struct {
				Digest map[string]string `json:"digest"`
			} `json:"subject"`
		}
		if json.Unmarshal(payload, &st) != nil {
			return false
		}
		alg, hex, _ := strings.Cut(imageDigest, ":")
		for _, s := range st.Subject {
			if s.Digest[alg] == hex {
				return true
			}
		}
	}
	return false
}
