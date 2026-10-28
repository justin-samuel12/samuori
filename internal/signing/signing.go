// Package signing signs published images with administrator-managed keys (ADR 0021). Signatures use Cosign's
// key-based "simple signing" format and provenance uses in-toto statements in DSSE envelopes, so standard tools can
// verify them. Both are stored in the destination repository under Cosign's tags and, where the registry supports
// it, as OCI 1.1 referrers (OD-25). Private keys are controller-only secrets and never reach workers.
package signing

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Media types and annotations of Cosign's signature and attestation images.
const (
	SimpleSigningType   = "application/vnd.dev.cosign.simplesigning.v1+json"
	DSSEType            = "application/vnd.dsse.envelope.v1+json"
	SignatureAnnotation = "dev.cosignproject.cosign/signature"
	PredicateAnnotation = "predicateType"
	InTotoPayloadType   = "application/vnd.in-toto+json"
	SLSAProvenanceV1    = "https://slsa.dev/provenance/v1"
	StatementV1         = "https://in-toto.io/Statement/v1"
	BuildType           = "https://samuori.invalid/build/v1"
)

var ErrBadSignature = errors.New("signing: the signature does not verify")

// Signer produces ECDSA signatures over SHA-256 digests in ASN.1 form, as Cosign verifies them. A key held by a KMS
// can implement it without changing callers (ADR 0021).
type Signer interface {
	Sign(data []byte) ([]byte, error)
	Capabilities() Capabilities
}

// Capabilities describes what a signer supports.
type Capabilities struct {
	KeyBased     bool
	Keyless      bool
	Attestations []string
}

// Key is a loaded ECDSA P-256 private key.
type Key struct {
	ID      string
	private *ecdsa.PrivateKey
}

// ParseKey reads a PKCS#8 PEM private key.
func ParseKey(id string, pemBytes []byte) (*Key, error) {
	b, _ := pem.Decode(pemBytes)
	if b == nil {
		return nil, errors.New("signing: the key is not PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(b.Bytes)
	if err != nil {
		return nil, err
	}
	ec, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("signing: only ECDSA keys are supported")
	}
	return &Key{ID: id, private: ec}, nil
}

func (k *Key) Sign(data []byte) ([]byte, error) {
	sum := sha256.Sum256(data)
	return ecdsa.SignASN1(rand.Reader, k.private, sum[:])
}

func (k *Key) Capabilities() Capabilities {
	return Capabilities{KeyBased: true, Attestations: []string{SLSAProvenanceV1}}
}

// PublicPEM returns the public key in the PEM form `cosign verify --key` reads.
func PublicPEM(pub crypto.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}

func (k *Key) PublicPEM() (string, error) { return PublicPEM(&k.private.PublicKey) }

// ParsePublic reads a PEM public key.
func ParsePublic(pemText string) (*ecdsa.PublicKey, error) {
	b, _ := pem.Decode([]byte(pemText))
	if b == nil {
		return nil, errors.New("signing: the public key is not PEM")
	}
	p, err := x509.ParsePKIXPublicKey(b.Bytes)
	if err != nil {
		return nil, err
	}
	ec, ok := p.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("signing: only ECDSA keys are supported")
	}
	return ec, nil
}

// Tag returns the tag Cosign uses for an image's signature (".sig") or attestations (".att").
func Tag(imageDigest, suffix string) string {
	return strings.Replace(imageDigest, ":", "-", 1) + "." + suffix
}

type simpleSigning struct {
	Critical struct {
		Identity struct {
			DockerReference string `json:"docker-reference"`
		} `json:"identity"`
		Image struct {
			DockerManifestDigest string `json:"docker-manifest-digest"`
		} `json:"image"`
		Type string `json:"type"`
	} `json:"critical"`
	Optional map[string]string `json:"optional"`
}

// Layer is one signature or attestation with its annotations.
type Layer struct {
	MediaType   string
	Data        []byte
	Annotations map[string]string
}

// Artifact is a small OCI image holding signatures or attestations, as Cosign stores them.
type Artifact struct {
	Config, Manifest []byte
	Layers           []Layer
}

// Blobs returns the config and layer contents to upload before the manifest.
func (a Artifact) Blobs() [][]byte {
	out := [][]byte{a.Config}
	for _, l := range a.Layers {
		out = append(out, l.Data)
	}
	return out
}

func layerDescriptors(layers []Layer) []map[string]any {
	out := []map[string]any{}
	for _, l := range layers {
		out = append(out, map[string]any{"mediaType": l.MediaType, "digest": digest(l.Data), "size": len(l.Data), "annotations": l.Annotations})
	}
	return out
}

// artifact builds the image Cosign reads from its ".sig" and ".att" tags.
func artifact(layers []Layer) (Artifact, error) {
	diffIDs := []string{}
	for _, l := range layers {
		diffIDs = append(diffIDs, digest(l.Data))
	}
	config, err := json.Marshal(map[string]any{
		"architecture": "", "os": "", "created": "0001-01-01T00:00:00Z", "config": map[string]any{},
		"rootfs": map[string]any{"type": "layers", "diff_ids": diffIDs},
	})
	if err != nil {
		return Artifact{}, err
	}
	manifest, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": digest(config), "size": len(config)},
		"layers":        layerDescriptors(layers),
	})
	if err != nil {
		return Artifact{}, err
	}
	return Artifact{Config: config, Manifest: manifest, Layers: layers}, nil
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Signature builds Cosign's signature image for reference (repository) at imageDigest.
func Signature(k Signer, reference, imageDigest string, optional map[string]string) (Artifact, error) {
	var p simpleSigning
	p.Critical.Identity.DockerReference = reference
	p.Critical.Image.DockerManifestDigest = imageDigest
	p.Critical.Type = "cosign container image signature"
	p.Optional = optional
	payload, err := json.Marshal(p)
	if err != nil {
		return Artifact{}, err
	}
	sig, err := k.Sign(payload)
	if err != nil {
		return Artifact{}, err
	}
	return artifact([]Layer{{MediaType: SimpleSigningType, Data: payload, Annotations: map[string]string{SignatureAnnotation: base64.StdEncoding.EncodeToString(sig)}}})
}

type manifest struct {
	Layers []struct {
		MediaType   string            `json:"mediaType"`
		Digest      string            `json:"digest"`
		Annotations map[string]string `json:"annotations"`
	} `json:"layers"`
}

// VerifySignature checks a Cosign signature image against pub, imageDigest and reference. fetch returns blobs.
func VerifySignature(pub *ecdsa.PublicKey, manifestBytes []byte, fetch func(digest string) ([]byte, error), reference, imageDigest string) error {
	var m manifest
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		return err
	}
	var mismatch error
	for _, l := range m.Layers {
		if l.MediaType != SimpleSigningType {
			continue
		}
		payload, err := fetch(l.Digest)
		if err != nil {
			return err
		}
		if digest(payload) != l.Digest {
			return fmt.Errorf("signing: the payload does not match its digest")
		}
		sig, err := base64.StdEncoding.DecodeString(l.Annotations[SignatureAnnotation])
		if err != nil {
			return ErrBadSignature
		}
		sum := sha256.Sum256(payload)
		if !ecdsa.VerifyASN1(pub, sum[:], sig) {
			continue
		}
		var p simpleSigning
		if err := json.Unmarshal(payload, &p); err != nil {
			return err
		}
		if p.Critical.Image.DockerManifestDigest != imageDigest || p.Critical.Identity.DockerReference != reference {
			mismatch = fmt.Errorf("signing: the signature is for %s@%s", p.Critical.Identity.DockerReference, p.Critical.Image.DockerManifestDigest)
			continue
		}
		return nil
	}
	if mismatch != nil {
		return mismatch
	}
	return ErrBadSignature
}

// Envelope is a DSSE envelope.
type Envelope struct {
	PayloadType string          `json:"payloadType"`
	Payload     string          `json:"payload"`
	Signatures  []DSSESignature `json:"signatures"`
}

// DSSESignature is one DSSE signature.
type DSSESignature struct {
	KeyID string `json:"keyid"`
	Sig   string `json:"sig"`
}

// pae is DSSE's pre-authentication encoding.
func pae(payloadType string, payload []byte) []byte {
	return []byte(fmt.Sprintf("DSSEv1 %d %s %d %s", len(payloadType), payloadType, len(payload), payload))
}

// Attestation wraps statement in a signed DSSE envelope stored as Cosign's attestation image.
func Attestation(k Signer, statement []byte, predicateType string) (Artifact, error) {
	if !slices.Contains(k.Capabilities().Attestations, predicateType) {
		return Artifact{}, fmt.Errorf("signing: the signer does not support %s attestations", predicateType)
	}
	sig, err := k.Sign(pae(InTotoPayloadType, statement))
	if err != nil {
		return Artifact{}, err
	}
	env, err := json.Marshal(Envelope{PayloadType: InTotoPayloadType, Payload: base64.StdEncoding.EncodeToString(statement), Signatures: []DSSESignature{{Sig: base64.StdEncoding.EncodeToString(sig)}}})
	if err != nil {
		return Artifact{}, err
	}
	// Cosign expects the signature annotation on attestation layers too, empty because DSSE carries the signature.
	return artifact([]Layer{{MediaType: DSSEType, Data: env, Annotations: map[string]string{PredicateAnnotation: predicateType, SignatureAnnotation: ""}}})
}

// VerifyAttestation checks a DSSE attestation image and returns the statement.
func VerifyAttestation(pub *ecdsa.PublicKey, manifestBytes []byte, fetch func(digest string) ([]byte, error)) ([]byte, error) {
	var m manifest
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		return nil, err
	}
	for _, l := range m.Layers {
		if l.MediaType != DSSEType {
			continue
		}
		raw, err := fetch(l.Digest)
		if err != nil {
			return nil, err
		}
		var env Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return nil, err
		}
		payload, err := base64.StdEncoding.DecodeString(env.Payload)
		if err != nil {
			return nil, err
		}
		for _, s := range env.Signatures {
			sig, err := base64.StdEncoding.DecodeString(s.Sig)
			if err != nil {
				continue
			}
			sum := sha256.Sum256(pae(env.PayloadType, payload))
			if ecdsa.VerifyASN1(pub, sum[:], sig) {
				return payload, nil
			}
		}
	}
	return nil, ErrBadSignature
}

// Provenance describes how Samuori built an image (SLSA provenance v1).
type Provenance struct {
	Subject          string
	ImageDigest      string
	BuilderID        string
	BuildRunID       string
	SpecID           string
	SpecRevision     int
	SpecDigest       string
	SnapshotDigest   string
	PlanDigest       string
	Method           string
	IsolationProfile string
	WorkerID, PoolID string
	Dependencies     map[string]string
	StartedOn        time.Time
	FinishedOn       time.Time
}

// Statement renders an in-toto statement with an SLSA provenance v1 predicate.
func (p Provenance) Statement() ([]byte, error) {
	deps := []map[string]any{}
	for name, d := range p.Dependencies {
		alg, hexd, _ := strings.Cut(d, ":")
		deps = append(deps, map[string]any{"name": name, "digest": map[string]string{alg: hexd}})
	}
	alg, hexd, _ := strings.Cut(p.ImageDigest, ":")
	return json.Marshal(map[string]any{
		"_type":         StatementV1,
		"subject":       []map[string]any{{"name": p.Subject, "digest": map[string]string{alg: hexd}}},
		"predicateType": SLSAProvenanceV1,
		"predicate": map[string]any{
			"buildDefinition": map[string]any{
				"buildType": BuildType,
				"externalParameters": map[string]any{
					"specId": p.SpecID, "specRevision": p.SpecRevision, "specDigest": p.SpecDigest, "sourceSnapshotDigest": p.SnapshotDigest, "method": p.Method,
				},
				"internalParameters":   map[string]any{"planDigest": p.PlanDigest, "isolationProfile": p.IsolationProfile},
				"resolvedDependencies": deps,
			},
			"runDetails": map[string]any{
				"builder":    map[string]any{"id": p.BuilderID},
				"metadata":   map[string]any{"invocationId": p.BuildRunID, "startedOn": p.StartedOn.UTC().Format(time.RFC3339), "finishedOn": p.FinishedOn.UTC().Format(time.RFC3339)},
				"byproducts": []map[string]any{{"name": "worker", "annotations": map[string]string{"workerId": p.WorkerID, "poolId": p.PoolID}}},
			},
		},
	})
}
