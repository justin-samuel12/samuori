package publication

import (
	"context"
	"crypto/ecdsa"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"oras.land/oras-go/v2/errdef"

	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/signing"
)

// UseSigning enables signatures and provenance attestations for destinations that ask for them (ADR 0021).
func (s *Service) UseSigning(st *signing.Store) { s.signing = st }

// BuilderID identifies Samuori as the builder in provenance.
const BuilderID = "https://samuori.invalid/builder/v1"

type signed struct {
	SignatureRef, AttestationRef, Storage string
}

// sign stores the image's signature and provenance at the destination and reads them back. New signatures are added
// to those already stored, and a promotion carries the source's signatures and attestations with it. Everything is
// written under Cosign's tags and, where the registry serves the OCI 1.1 referrers API, also as referrers (OD-25).
func (s *Service) sign(ctx context.Context, pub Publication, d Destination, t Target, image string) (signed, error) {
	carried, carriedFrom, err := s.carried(ctx, pub, image)
	if err != nil {
		return signed{}, err
	}
	if !d.SignImages && !d.AttachProvenance && len(carried) == 0 {
		return signed{}, nil
	}
	subject, err := s.pusher.Describe(ctx, t, image)
	if err != nil {
		return signed{}, fmt.Errorf("reading the image back: %w", err)
	}
	referrers, _, err := s.pusher.Referrers(ctx, t, image)
	if err != nil {
		return signed{}, err
	}
	out := signed{Storage: signing.StorageTag}
	if referrers {
		out.Storage = signing.StorageReferrers
	}
	reference := t.Endpoint + "/" + t.Repository
	var own []signing.Layer
	var ownChecks, carriedChecks []sigCheck
	if len(carried) > 0 {
		if carriedChecks, err = s.orgChecks(ctx, pub.OrgID, carriedFrom); err != nil {
			return signed{}, err
		}
	}
	if d.SignImages || d.AttachProvenance {
		if s.signing == nil {
			return signed{}, signing.ErrNoKey
		}
		key, info, err := s.signing.Active(ctx, pub.OrgID)
		if err != nil {
			return signed{}, err
		}
		pubKey, err := signing.ParsePublic(info.PublicKey)
		if err != nil {
			return signed{}, err
		}
		ownChecks = []sigCheck{{pubKey, reference}}
		if d.SignImages {
			a, err := signing.Signature(key, reference, image, map[string]string{"samuori.publication": pub.ID, "samuori.signingKey": info.Name})
			if err != nil {
				return signed{}, err
			}
			own = append(own, a.Layers...)
		}
		if d.AttachProvenance {
			prov, err := s.provenance(ctx, pub, reference, image)
			if err != nil {
				return signed{}, err
			}
			stmt, err := prov.Statement()
			if err != nil {
				return signed{}, err
			}
			a, err := signing.Attestation(key, stmt, signing.SLSAProvenanceV1)
			if err != nil {
				return signed{}, err
			}
			own = append(own, a.Layers...)
		}
	}
	for _, kind := range []struct{ suffix, mediaType string }{{"sig", signing.SimpleSigningType}, {"att", signing.DSSEType}} {
		mine, theirs := ofType(own, kind.mediaType), ofType(carried, kind.mediaType)
		if len(mine)+len(theirs) == 0 {
			continue
		}
		tag := signing.Tag(image, kind.suffix)
		existing, err := s.storedLayers(ctx, t, tag, image)
		if err != nil {
			return signed{}, err
		}
		combined, err := signing.Combine(slices.Concat(mine, theirs, existing)...)
		if err != nil {
			return signed{}, err
		}
		ref, err := s.pusher.PushArtifact(ctx, t, combined, tag)
		if err != nil {
			return signed{}, fmt.Errorf("pushing %s: %w", tag, err)
		}
		if referrers {
			for i, l := range slices.Concat(mine, theirs) {
				r, err := signing.Referrer(l, subject)
				if err != nil {
					return signed{}, err
				}
				d, err := s.pusher.PushArtifact(ctx, t, r, "")
				if err != nil {
					return signed{}, fmt.Errorf("pushing a referrer: %w", err)
				}
				if i == 0 {
					ref = d
				}
			}
		}
		// Our own new signature must verify; carried ones are checked against the organization's keys.
		checks := ownChecks
		if len(mine) == 0 {
			checks = carriedChecks
		}
		if err := s.verifyStored(ctx, t, tag, image, kind.mediaType, checks, referrers); err != nil {
			return signed{}, err
		}
		if kind.suffix == "sig" {
			out.SignatureRef = reference + "@" + ref
		} else {
			out.AttestationRef = reference + "@" + ref
		}
	}
	return out, nil
}

// sigCheck is a key and the repository a signature by it must name.
type sigCheck struct {
	key       *ecdsa.PublicKey
	reference string
}

func ofType(layers []signing.Layer, mediaType string) []signing.Layer {
	var out []signing.Layer
	for _, l := range layers {
		if l.MediaType == mediaType {
			out = append(out, l)
		}
	}
	return out
}

// storedLayers returns what a Cosign tag already holds for image, or nothing when the tag does not exist.
func (s *Service) storedLayers(ctx context.Context, t Target, tag, image string) ([]signing.Layer, error) {
	m, fetch, err := s.pusher.FetchTag(ctx, t, tag)
	if errors.Is(err, errdef.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", tag, err)
	}
	layers, err := signing.ReadLayers(m, fetch)
	if err != nil {
		return nil, err
	}
	var out []signing.Layer
	for _, l := range layers {
		if signing.Covers(l, image) {
			out = append(out, l)
		}
	}
	return out, nil
}

// verifyStored reads the tag back, and the referrers where they are used, and requires a signature or attestation
// that verifies with one of checks in each.
func (s *Service) verifyStored(ctx context.Context, t Target, tag, image, mediaType string, checks []sigCheck, referrers bool) error {
	m, fetch, err := s.pusher.FetchTag(ctx, t, tag)
	if err != nil {
		return fmt.Errorf("reading %s back: %w", tag, err)
	}
	if !verifies(m, fetch, mediaType, checks, image) {
		return fmt.Errorf("nothing under %s verifies", tag)
	}
	if !referrers {
		return nil
	}
	_, refs, err := s.pusher.Referrers(ctx, t, image)
	if err != nil {
		return fmt.Errorf("listing referrers: %w", err)
	}
	for _, r := range refs {
		if signing.ArtifactType(r) == mediaType && verifies(r, fetch, mediaType, checks, image) {
			return nil
		}
	}
	return fmt.Errorf("no %s referrer of the image verifies", mediaType)
}

func verifies(m []byte, fetch func(string) ([]byte, error), mediaType string, checks []sigCheck, image string) bool {
	for _, c := range checks {
		if mediaType == signing.SimpleSigningType {
			if signing.VerifySignature(c.key, m, fetch, c.reference, image) == nil {
				return true
			}
		} else if _, err := signing.VerifyAttestation(c.key, m, fetch); err == nil {
			return true
		}
	}
	return false
}

// provenance gathers what Samuori recorded about the build: inputs by digest, the executing worker and pool
// (SEC-WRK-06), the isolation profile and the recipe images.
func (s *Service) provenance(ctx context.Context, pub Publication, reference, image string) (signing.Provenance, error) {
	p := signing.Provenance{Subject: reference, ImageDigest: image, BuilderID: BuilderID, Dependencies: map[string]string{}}
	var planDigest sql.NullString
	var ended sql.NullTime
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT r.id, r.spec_id, r.spec_revision, r.spec_digest, r.snapshot_digest, r.plan_digest, r.isolation_profile, t.worker_id, t.pool_id, t.started_at, t.ended_at, r.updated_at
		FROM build_artifacts a JOIN build_runs r ON r.id = a.run_id JOIN build_attempts t ON t.id = a.attempt_id WHERE a.id = ?`), pub.ArtifactID).
		Scan(&p.BuildRunID, &p.SpecID, &p.SpecRevision, &p.SpecDigest, &p.SnapshotDigest, &planDigest, &p.IsolationProfile, &p.WorkerID, &p.PoolID, &p.StartedOn, &ended, &p.FinishedOn)
	if err != nil {
		return p, fmt.Errorf("the build record for provenance: %w", err)
	}
	if ended.Valid {
		p.FinishedOn = ended.Time
	}
	p.PlanDigest = planDigest.String
	var raw string
	if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT plan FROM build_plans WHERE digest = ?`), p.PlanDigest).Scan(&raw); err == nil {
		if plan, err := buildplan.Decode([]byte(raw)); err == nil {
			p.Method = plan.Method
			if r := plan.Recipe; r != nil {
				images := []string{r.BuildImage, r.RunImage}
				if r.Frontend != nil {
					images = append(images, r.Frontend.Image)
				}
				for _, img := range images {
					if name, d, ok := splitDigest(img); ok {
						p.Dependencies[name] = d
					}
				}
			}
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return p, err
	}
	if p.StartedOn.IsZero() {
		p.StartedOn = time.Now()
	}
	return p, nil
}

func splitDigest(ref string) (string, string, bool) {
	for i := len(ref) - 1; i >= 0; i-- {
		if ref[i] == '@' {
			return ref[:i], ref[i+1:], true
		}
	}
	return "", "", false
}

// verifySource requires a signature on the promoted image, at the source destination, by one of the organization's
// keys. An unsigned or unverifiable source is never promoted (OD-25).
func (s *Service) verifySource(ctx context.Context, src Publication) error {
	if s.signing == nil {
		return signing.ErrNoKey
	}
	refs, err := s.chainRefs(ctx, src)
	if err != nil {
		return err
	}
	checks, err := s.orgChecks(ctx, src.OrgID, refs)
	if err != nil {
		return err
	}
	d, err := s.destination(ctx, src.OrgID, src.DestinationID)
	if err != nil {
		return err
	}
	t, _, err := s.target(ctx, src.OrgID, d)
	if err != nil {
		return err
	}
	manifest, fetch, err := s.pusher.FetchTag(ctx, t, signing.Tag(src.ImageDigest, "sig"))
	if errors.Is(err, errdef.ErrNotFound) {
		return errors.New("the source image is not signed")
	}
	if err != nil {
		return fmt.Errorf("the source signature could not be read: %w", err)
	}
	if verifies(manifest, fetch, signing.SimpleSigningType, checks, src.ImageDigest) {
		return nil
	}
	return signing.ErrBadSignature
}

// carried returns the signatures and attestations a promotion copies from its source, and the repositories they
// may name.
func (s *Service) carried(ctx context.Context, pub Publication, image string) ([]signing.Layer, []string, error) {
	if pub.Kind != "promote" {
		return nil, nil, nil
	}
	src, err := s.load(ctx, pub.SourcePublicationID)
	if err != nil {
		return nil, nil, err
	}
	d, err := s.destination(ctx, src.OrgID, src.DestinationID)
	if err != nil {
		return nil, nil, err
	}
	t, _, err := s.target(ctx, src.OrgID, d)
	if err != nil {
		return nil, nil, err
	}
	var out []signing.Layer
	for _, suffix := range []string{"sig", "att"} {
		layers, err := s.storedLayers(ctx, t, signing.Tag(image, suffix), image)
		if err != nil {
			return nil, nil, fmt.Errorf("reading the source's signatures: %w", err)
		}
		out = append(out, layers...)
	}
	refs, err := s.chainRefs(ctx, src)
	return out, refs, err
}

// chainRefs lists the repositories of a publication and of the publications it was promoted from.
func (s *Service) chainRefs(ctx context.Context, p Publication) ([]string, error) {
	var refs []string
	for range 16 {
		if repo, _, ok := strings.Cut(p.DestinationRef, "@"); ok && !slices.Contains(refs, repo) {
			refs = append(refs, repo)
		}
		if p.Kind != "promote" || p.SourcePublicationID == "" {
			break
		}
		next, err := s.load(ctx, p.SourcePublicationID)
		if err != nil {
			return nil, err
		}
		p = next
	}
	return refs, nil
}

func (s *Service) orgChecks(ctx context.Context, orgID string, refs []string) ([]sigCheck, error) {
	if s.signing == nil {
		return nil, signing.ErrNoKey
	}
	keys, err := s.signing.List(ctx, orgID)
	if err != nil {
		return nil, err
	}
	var out []sigCheck
	for _, k := range keys {
		pk, err := signing.ParsePublic(k.PublicKey)
		if err != nil {
			continue
		}
		for _, r := range refs {
			out = append(out, sigCheck{pk, r})
		}
	}
	return out, nil
}
