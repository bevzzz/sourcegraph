package internal

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"testing"
	"time"

	mockassert "github.com/derision-test/go-mockgen/testutil/assert"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/sourcegraph/log/logtest"

	"github.com/sourcegraph/sourcegraph/cmd/gitserver/internal/common"
	"github.com/sourcegraph/sourcegraph/cmd/gitserver/internal/git"
	"github.com/sourcegraph/sourcegraph/internal/actor"
	"github.com/sourcegraph/sourcegraph/internal/api"
	"github.com/sourcegraph/sourcegraph/internal/authz"
	"github.com/sourcegraph/sourcegraph/internal/gitserver"
	"github.com/sourcegraph/sourcegraph/internal/gitserver/gitdomain"
	"github.com/sourcegraph/sourcegraph/internal/gitserver/protocol"
	proto "github.com/sourcegraph/sourcegraph/internal/gitserver/v1"
	v1 "github.com/sourcegraph/sourcegraph/internal/gitserver/v1"
	internalgrpc "github.com/sourcegraph/sourcegraph/internal/grpc"
	"github.com/sourcegraph/sourcegraph/internal/grpc/defaults"
)

func TestGRPCServer_Blame(t *testing.T) {
	mockSS := gitserver.NewMockGitserverService_BlameServer()
	// Add an actor to the context.
	a := actor.FromUser(1)
	mockSS.ContextFunc.SetDefaultReturn(actor.WithActor(context.Background(), a))
	t.Run("argument validation", func(t *testing.T) {
		gs := &grpcServer{}
		err := gs.Blame(&v1.BlameRequest{RepoName: "", Path: "thepath"}, mockSS)
		require.ErrorContains(t, err, "repo must be specified")
		assertGRPCStatusCode(t, err, codes.InvalidArgument)
		err = gs.Blame(&v1.BlameRequest{RepoName: "therepo", Path: ""}, mockSS)
		require.ErrorContains(t, err, "path must be specified")
		assertGRPCStatusCode(t, err, codes.InvalidArgument)
	})
	t.Run("checks for uncloned repo", func(t *testing.T) {
		svc := NewMockService()
		svc.MaybeStartCloneFunc.SetDefaultReturn(&protocol.NotFoundPayload{CloneInProgress: true, CloneProgress: "cloning"}, false)
		gs := &grpcServer{svc: svc}
		err := gs.Blame(&v1.BlameRequest{RepoName: "therepo", Path: "thepath"}, mockSS)
		require.Error(t, err)
		assertGRPCStatusCode(t, err, codes.NotFound)
		assertHasGRPCErrorDetailOfType(t, err, &proto.RepoNotFoundPayload{})
		require.Contains(t, err.Error(), "repo not cloned")
		mockassert.Called(t, svc.MaybeStartCloneFunc)
	})
	t.Run("checks for subrepo perms access to given path", func(t *testing.T) {
		srp := authz.NewMockSubRepoPermissionChecker()
		svc := NewMockService()
		// Repo is cloned, proceed!
		svc.MaybeStartCloneFunc.SetDefaultReturn(nil, true)
		gs := &grpcServer{
			subRepoChecker: srp,
			svc:            svc,
			getBackendFunc: func(common.GitDir, api.RepoName) git.GitBackend {
				b := git.NewMockGitBackend()
				hr := git.NewMockBlameHunkReader()
				hr.ReadFunc.SetDefaultReturn(nil, io.EOF)
				b.BlameFunc.SetDefaultReturn(hr, nil)
				return b
			},
		}

		t.Run("subrepo perms are not enabled", func(t *testing.T) {
			srp.EnabledFunc.SetDefaultReturn(false)
			err := gs.Blame(&v1.BlameRequest{RepoName: "therepo", Path: "thepath"}, mockSS)
			assert.NoError(t, err)
			mockassert.Called(t, srp.EnabledFunc)
		})

		t.Run("subrepo perms are enabled, permission granted", func(t *testing.T) {
			srp.EnabledFunc.SetDefaultReturn(true)
			srp.PermissionsFunc.SetDefaultReturn(authz.Read, nil)
			err := gs.Blame(&v1.BlameRequest{RepoName: "therepo", Path: "thepath"}, mockSS)
			assert.NoError(t, err)
			mockassert.Called(t, srp.EnabledFunc)
			mockassert.Called(t, srp.PermissionsFunc)
		})

		t.Run("subrepo perms are enabled, permission denied", func(t *testing.T) {
			srp.EnabledFunc.SetDefaultReturn(true)
			srp.PermissionsFunc.SetDefaultReturn(authz.None, nil)
			err := gs.Blame(&v1.BlameRequest{RepoName: "therepo", Path: "thepath"}, mockSS)
			require.Error(t, err)
			mockassert.Called(t, srp.EnabledFunc)
			mockassert.Called(t, srp.PermissionsFunc)
			assertGRPCStatusCode(t, err, codes.PermissionDenied)
		})
	})
	t.Run("e2e", func(t *testing.T) {
		srp := authz.NewMockSubRepoPermissionChecker()
		// Skip subrepo perms checks.
		srp.EnabledFunc.SetDefaultReturn(false)
		svc := NewMockService()
		// Repo is cloned, proceed!
		svc.MaybeStartCloneFunc.SetDefaultReturn(nil, true)
		b := git.NewMockGitBackend()
		hr := git.NewMockBlameHunkReader()
		hr.ReadFunc.PushReturn(&gitdomain.Hunk{CommitID: "deadbeef"}, nil)
		hr.ReadFunc.PushReturn(nil, io.EOF)
		b.BlameFunc.SetDefaultReturn(hr, nil)
		gs := &grpcServer{
			subRepoChecker: srp,
			svc:            svc,
			getBackendFunc: func(common.GitDir, api.RepoName) git.GitBackend {
				return b
			},
		}

		cli := spawnServer(t, gs)
		r, err := cli.Blame(context.Background(), &v1.BlameRequest{
			RepoName: "therepo",
			Path:     "thepath",
		})
		require.NoError(t, err)
		for {
			msg, err := r.Recv()
			if err != nil {
				if err == io.EOF {
					return
				}
				require.NoError(t, err)
			}
			if diff := cmp.Diff(&proto.BlameResponse{
				Hunk: &proto.BlameHunk{
					Commit: "deadbeef",
					Author: &v1.BlameAuthor{
						Date: timestamppb.New(time.Time{}),
					},
				},
			}, msg, cmpopts.IgnoreUnexported(proto.BlameResponse{}, proto.BlameHunk{}, proto.BlameAuthor{}, timestamppb.Timestamp{})); diff != "" {
				t.Fatalf("unexpected response (-want +got):\n%s", diff)
			}
		}
	})
}

func TestGRPCServer_DefaultBranch(t *testing.T) {
	ctx := context.Background()
	t.Run("argument validation", func(t *testing.T) {
		gs := &grpcServer{}
		_, err := gs.DefaultBranch(ctx, &v1.DefaultBranchRequest{RepoName: ""})
		require.ErrorContains(t, err, "repo must be specified")
		assertGRPCStatusCode(t, err, codes.InvalidArgument)
	})
	t.Run("checks for uncloned repo", func(t *testing.T) {
		svc := NewMockService()
		svc.MaybeStartCloneFunc.SetDefaultReturn(&protocol.NotFoundPayload{CloneInProgress: true, CloneProgress: "cloning"}, false)
		gs := &grpcServer{svc: svc}
		_, err := gs.DefaultBranch(ctx, &v1.DefaultBranchRequest{RepoName: "therepo"})
		require.Error(t, err)
		assertGRPCStatusCode(t, err, codes.NotFound)
		assertHasGRPCErrorDetailOfType(t, err, &proto.RepoNotFoundPayload{})
		require.Contains(t, err.Error(), "repo not cloned")
		mockassert.Called(t, svc.MaybeStartCloneFunc)
	})
	t.Run("e2e", func(t *testing.T) {
		svc := NewMockService()
		// Repo is cloned, proceed!
		svc.MaybeStartCloneFunc.SetDefaultReturn(nil, true)
		b := git.NewMockGitBackend()
		b.SymbolicRefHeadFunc.SetDefaultReturn("refs/heads/main", nil)
		b.RevParseHeadFunc.SetDefaultReturn("deadbeef", nil)
		gs := &grpcServer{
			svc: svc,
			getBackendFunc: func(common.GitDir, api.RepoName) git.GitBackend {
				return b
			},
		}

		cli := spawnServer(t, gs)
		res, err := cli.DefaultBranch(ctx, &v1.DefaultBranchRequest{
			RepoName: "therepo",
		})
		require.NoError(t, err)
		if diff := cmp.Diff(&proto.DefaultBranchResponse{
			RefName: "refs/heads/main",
			Commit:  "deadbeef",
		}, res, cmpopts.IgnoreUnexported(proto.DefaultBranchResponse{})); diff != "" {
			t.Fatalf("unexpected response (-want +got):\n%s", diff)
		}

		// Check that RevNotFoundErrors are mapped correctly:
		b.RevParseHeadFunc.SetDefaultReturn("", &gitdomain.RevisionNotFoundError{Repo: "therepo", Spec: "HEAD"})
		_, err = cli.DefaultBranch(ctx, &v1.DefaultBranchRequest{
			RepoName: "therepo",
		})
		require.Error(t, err)
		assertGRPCStatusCode(t, err, codes.NotFound)
		assertHasGRPCErrorDetailOfType(t, err, &proto.RevisionNotFoundPayload{})
	})
}

func TestGRPCServer_MergeBase(t *testing.T) {
	ctx := context.Background()
	t.Run("argument validation", func(t *testing.T) {
		gs := &grpcServer{}
		_, err := gs.MergeBase(ctx, &v1.MergeBaseRequest{RepoName: ""})
		require.ErrorContains(t, err, "repo must be specified")
		assertGRPCStatusCode(t, err, codes.InvalidArgument)
		_, err = gs.MergeBase(ctx, &v1.MergeBaseRequest{RepoName: "therepo", Base: []byte{}})
		require.ErrorContains(t, err, "base must be specified")
		assertGRPCStatusCode(t, err, codes.InvalidArgument)
		_, err = gs.MergeBase(ctx, &v1.MergeBaseRequest{RepoName: "therepo", Base: []byte("master")})
		require.ErrorContains(t, err, "head must be specified")
		assertGRPCStatusCode(t, err, codes.InvalidArgument)
	})
	t.Run("checks for uncloned repo", func(t *testing.T) {
		svc := NewMockService()
		svc.MaybeStartCloneFunc.SetDefaultReturn(&protocol.NotFoundPayload{CloneInProgress: true, CloneProgress: "cloning"}, false)
		gs := &grpcServer{svc: svc}
		_, err := gs.MergeBase(ctx, &v1.MergeBaseRequest{RepoName: "therepo", Base: []byte("master"), Head: []byte("b2")})
		require.Error(t, err)
		assertGRPCStatusCode(t, err, codes.NotFound)
		assertHasGRPCErrorDetailOfType(t, err, &proto.RepoNotFoundPayload{})
		require.Contains(t, err.Error(), "repo not cloned")
		mockassert.Called(t, svc.MaybeStartCloneFunc)
	})
	t.Run("e2e", func(t *testing.T) {
		svc := NewMockService()
		// Repo is cloned, proceed!
		svc.MaybeStartCloneFunc.SetDefaultReturn(nil, true)
		b := git.NewMockGitBackend()
		b.MergeBaseFunc.SetDefaultReturn("deadbeef", nil)
		gs := &grpcServer{
			svc: svc,
			getBackendFunc: func(common.GitDir, api.RepoName) git.GitBackend {
				return b
			},
		}

		cli := spawnServer(t, gs)
		res, err := cli.MergeBase(ctx, &v1.MergeBaseRequest{
			RepoName: "therepo",
			Base:     []byte("master"),
			Head:     []byte("b2"),
		})
		require.NoError(t, err)
		if diff := cmp.Diff(&proto.MergeBaseResponse{
			MergeBaseCommitSha: "deadbeef",
		}, res, cmpopts.IgnoreUnexported(proto.MergeBaseResponse{})); diff != "" {
			t.Fatalf("unexpected response (-want +got):\n%s", diff)
		}
	})
}

func TestGRPCServer_ReadFile(t *testing.T) {
	mockSS := gitserver.NewMockGitserverService_ReadFileServer()
	// Add an actor to the context.
	a := actor.FromUser(1)
	mockSS.ContextFunc.SetDefaultReturn(actor.WithActor(context.Background(), a))
	t.Run("argument validation", func(t *testing.T) {
		gs := &grpcServer{}
		err := gs.ReadFile(&v1.ReadFileRequest{RepoName: ""}, mockSS)
		require.ErrorContains(t, err, "repo must be specified")
		assertGRPCStatusCode(t, err, codes.InvalidArgument)
		err = gs.ReadFile(&v1.ReadFileRequest{RepoName: "therepo", Path: ""}, mockSS)
		require.ErrorContains(t, err, "path must be specified")
		assertGRPCStatusCode(t, err, codes.InvalidArgument)
		err = gs.ReadFile(&v1.ReadFileRequest{RepoName: "therepo", Path: "thepath", Commit: ""}, mockSS)
		require.ErrorContains(t, err, "commit must be specified")
		assertGRPCStatusCode(t, err, codes.InvalidArgument)
	})
	t.Run("checks for uncloned repo", func(t *testing.T) {
		svc := NewMockService()
		svc.MaybeStartCloneFunc.SetDefaultReturn(&protocol.NotFoundPayload{CloneInProgress: true, CloneProgress: "cloning"}, false)
		gs := &grpcServer{svc: svc}
		err := gs.ReadFile(&v1.ReadFileRequest{RepoName: "therepo", Commit: "deadbeef", Path: "thepath"}, mockSS)
		require.Error(t, err)
		assertGRPCStatusCode(t, err, codes.NotFound)
		assertHasGRPCErrorDetailOfType(t, err, &proto.RepoNotFoundPayload{})
		require.Contains(t, err.Error(), "repo not cloned")
		mockassert.Called(t, svc.MaybeStartCloneFunc)
	})
	t.Run("checks for subrepo perms access to given path", func(t *testing.T) {
		srp := authz.NewMockSubRepoPermissionChecker()
		svc := NewMockService()
		// Repo is cloned, proceed!
		svc.MaybeStartCloneFunc.SetDefaultReturn(nil, true)
		gs := &grpcServer{
			subRepoChecker: srp,
			svc:            svc,
			getBackendFunc: func(common.GitDir, api.RepoName) git.GitBackend {
				b := git.NewMockGitBackend()
				b.ReadFileFunc.SetDefaultReturn(io.NopCloser(bytes.NewReader([]byte("filecontent"))), nil)
				return b
			},
		}

		t.Run("subrepo perms are not enabled", func(t *testing.T) {
			srp.EnabledFunc.SetDefaultReturn(false)
			err := gs.ReadFile(&v1.ReadFileRequest{RepoName: "therepo", Commit: "deadbeef", Path: "thepath"}, mockSS)
			assert.NoError(t, err)
			mockassert.Called(t, srp.EnabledFunc)
		})

		t.Run("subrepo perms are enabled, permission granted", func(t *testing.T) {
			srp.EnabledFunc.SetDefaultReturn(true)
			srp.PermissionsFunc.SetDefaultReturn(authz.Read, nil)
			err := gs.ReadFile(&v1.ReadFileRequest{RepoName: "therepo", Commit: "deadbeef", Path: "thepath"}, mockSS)
			assert.NoError(t, err)
			mockassert.Called(t, srp.EnabledFunc)
			mockassert.Called(t, srp.PermissionsFunc)
		})

		t.Run("subrepo perms are enabled, permission denied", func(t *testing.T) {
			srp.EnabledFunc.SetDefaultReturn(true)
			srp.PermissionsFunc.SetDefaultReturn(authz.None, nil)
			err := gs.ReadFile(&v1.ReadFileRequest{RepoName: "therepo", Commit: "deadbeef", Path: "thepath"}, mockSS)
			require.Error(t, err)
			mockassert.Called(t, srp.EnabledFunc)
			mockassert.Called(t, srp.PermissionsFunc)
			assertGRPCStatusCode(t, err, codes.PermissionDenied)
		})
	})
	t.Run("e2e", func(t *testing.T) {
		srp := authz.NewMockSubRepoPermissionChecker()
		// Skip subrepo perms checks.
		srp.EnabledFunc.SetDefaultReturn(false)
		svc := NewMockService()
		// Repo is cloned, proceed!
		svc.MaybeStartCloneFunc.SetDefaultReturn(nil, true)
		b := git.NewMockGitBackend()
		b.ReadFileFunc.SetDefaultReturn(io.NopCloser(bytes.NewReader([]byte("filecontent"))), nil)
		gs := &grpcServer{
			subRepoChecker: srp,
			svc:            svc,
			getBackendFunc: func(common.GitDir, api.RepoName) git.GitBackend {
				return b
			},
		}

		cli := spawnServer(t, gs)
		r, err := cli.ReadFile(context.Background(), &v1.ReadFileRequest{
			RepoName: "therepo",
			Commit:   "deadbeef",
			Path:     "thepath",
		})
		require.NoError(t, err)
		for {
			msg, err := r.Recv()
			if err != nil {
				if err == io.EOF {
					break
				}
				require.NoError(t, err)
			}
			if diff := cmp.Diff(&proto.ReadFileResponse{
				Data: []byte("filecontent"),
			}, msg, cmpopts.IgnoreUnexported(proto.ReadFileResponse{})); diff != "" {
				t.Fatalf("unexpected response (-want +got):\n%s", diff)
			}
		}

		b.ReadFileFunc.SetDefaultReturn(nil, os.ErrNotExist)
		cc, err := cli.ReadFile(context.Background(), &v1.ReadFileRequest{
			RepoName: "therepo",
			Commit:   "deadbeef",
			Path:     "thepath",
		})
		require.NoError(t, err)
		_, err = cc.Recv()
		require.Error(t, err)
		assertGRPCStatusCode(t, err, codes.NotFound)
		assertHasGRPCErrorDetailOfType(t, err, &proto.FileNotFoundPayload{})

		b.ReadFileFunc.SetDefaultReturn(nil, &gitdomain.RevisionNotFoundError{})
		cc, err = cli.ReadFile(context.Background(), &v1.ReadFileRequest{
			RepoName: "therepo",
			Commit:   "deadbeef",
			Path:     "thepath",
		})
		require.NoError(t, err)
		_, err = cc.Recv()
		require.Error(t, err)
		assertGRPCStatusCode(t, err, codes.NotFound)
		assertHasGRPCErrorDetailOfType(t, err, &proto.RevisionNotFoundPayload{})
	})
}

func assertGRPCStatusCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	s, ok := status.FromError(err)
	require.True(t, ok, "expected status.FromError to succeed")
	require.Equal(t, want, s.Code())
}

func assertHasGRPCErrorDetailOfType(t *testing.T, err error, typ any) {
	t.Helper()
	s, ok := status.FromError(err)
	require.True(t, ok, "expected status.FromError to succeed")
	for _, d := range s.Details() {
		// Compare types of d and typ:
		if reflect.TypeOf(d) == reflect.TypeOf(typ) {
			return
		}
	}
	t.Fatalf("error %v does not implement error detail type %T", err, typ)
}

func spawnServer(t *testing.T, server *grpcServer) proto.GitserverServiceClient {
	t.Helper()
	grpcServer := defaults.NewServer(logtest.Scoped(t))
	proto.RegisterGitserverServiceServer(grpcServer, server)
	handler := internalgrpc.MultiplexHandlers(grpcServer, http.NotFoundHandler())
	srv := httptest.NewServer(handler)
	t.Cleanup(func() {
		srv.Close()
	})

	u, err := url.Parse(srv.URL)
	require.NoError(t, err)

	cc, err := defaults.Dial(u.Host, logtest.Scoped(t))
	require.NoError(t, err)

	return proto.NewGitserverServiceClient(cc)
}
