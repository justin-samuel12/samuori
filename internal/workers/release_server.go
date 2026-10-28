package workers

import (
	"context"
	"errors"
	"io"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	workerv1 "samuori.invalid/samuori/internal/workerproto/workerv1"
)

// ErrNotUpdateTarget means no running rollout is waiting for this worker to fetch that release.
var ErrNotUpdateTarget = errors.New("this worker is not the current target of a self-update rollout")

// Releases serves self-update releases to the worker a rollout targets (ADR 0044).
type Releases interface {
	OpenRelease(ctx context.Context, workerID, version, arch string) (checksums, signature []byte, archive io.ReadCloser, err error)
	ReportUpdate(ctx context.Context, workerID, version, outcome, detail string) error
}

func (s *Server) UseReleases(r Releases) { s.releases = r }

func (s *Server) GetRelease(req *workerv1.GetReleaseRequest, stream grpc.ServerStreamingServer[workerv1.ReleaseChunk]) error {
	if s.releases == nil {
		return status.Error(codes.Unavailable, "self-update is not enabled on this controller")
	}
	w := stream.Context().Value(workerKey{}).(Worker)
	sums, sig, archive, err := s.releases.OpenRelease(stream.Context(), w.ID, req.GetVersion(), req.GetArch())
	if errors.Is(err, ErrNotUpdateTarget) {
		return status.Error(codes.PermissionDenied, err.Error())
	}
	if err != nil {
		return s.callError("GetRelease", err)
	}
	defer archive.Close()
	if err := stream.Send(&workerv1.ReleaseChunk{Kind: &workerv1.ReleaseChunk_Checksums{Checksums: sums}}); err != nil {
		return err
	}
	if err := stream.Send(&workerv1.ReleaseChunk{Kind: &workerv1.ReleaseChunk_Signature{Signature: sig}}); err != nil {
		return err
	}
	buf := make([]byte, 1<<20)
	for {
		n, err := archive.Read(buf)
		if n > 0 {
			if serr := stream.Send(&workerv1.ReleaseChunk{Kind: &workerv1.ReleaseChunk_Data{Data: buf[:n]}}); serr != nil {
				return serr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return s.callError("GetRelease", err)
		}
	}
}

func (s *Server) ReportUpdate(ctx context.Context, req *workerv1.UpdateReport) (*workerv1.UpdateReportAck, error) {
	if s.releases == nil {
		return nil, status.Error(codes.Unavailable, "self-update is not enabled on this controller")
	}
	w := ctx.Value(workerKey{}).(Worker)
	if err := s.releases.ReportUpdate(ctx, w.ID, req.GetVersion(), req.GetOutcome(), req.GetDetail()); err != nil {
		return nil, s.callError("ReportUpdate", err)
	}
	return &workerv1.UpdateReportAck{}, nil
}
