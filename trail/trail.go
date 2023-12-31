package trail

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"os"

	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/khulnasoft-lab/trace"
	"github.com/khulnasoft-lab/trace/internal"
)

// Send is a high level function that:
// * converts error to GRPC error
// * attaches debug metadata to existing metadata if possible
// * sends the header to GRPC
func Send(ctx context.Context, err error) error {
	meta, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		meta = metadata.New(nil)
	}
	if trace.IsDebug() {
		SetDebugInfo(err, meta)
	}
	if len(meta) != 0 {
		sendErr := grpc.SendHeader(ctx, meta)
		if sendErr != nil {
			return trace.NewAggregate(err, sendErr)
		}
	}
	return ToGRPC(err)
}

// DebugReportMetadata is a key in metadata holding debug information
// about the error - stack traces and original error
const DebugReportMetadata = "trace-debug-report"

// ToGRPC converts error to GRPC-compatible error
func ToGRPC(originalErr error) error {
	if originalErr == nil {
		return nil
	}

	// Avoid modifying top-level gRPC errors.
	if _, ok := status.FromError(originalErr); ok {
		return originalErr
	}

	code := codes.Unknown
	returnOriginal := false
	internal.TraverseErr(originalErr, func(err error) (ok bool) {
		if err == io.EOF {
			// Keep legacy semantics and return the original error.
			returnOriginal = true
			return true
		}

		if s, ok := status.FromError(err); ok {
			code = s.Code()
			return true
		}

		// Duplicate check from trace.IsNotFound.
		if os.IsNotExist(err) {
			code = codes.NotFound
			return true
		}

		ok = true // Assume match
		switch err.(type) {
		case *trace.AccessDeniedError:
			code = codes.PermissionDenied
		case *trace.AlreadyExistsError:
			code = codes.AlreadyExists
		case *trace.BadParameterError:
			code = codes.InvalidArgument
		case *trace.CompareFailedError:
			code = codes.FailedPrecondition
		case *trace.ConnectionProblemError:
			code = codes.Unavailable
		case *trace.LimitExceededError:
			code = codes.ResourceExhausted
		case *trace.NotFoundError:
			code = codes.NotFound
		case *trace.NotImplementedError:
			code = codes.Unimplemented
		case *trace.OAuth2Error:
			code = codes.InvalidArgument
		// *trace.RetryError not mapped.
		// *trace.TrustError not mapped.
		default:
			ok = false
		}
		return ok
	})
	if returnOriginal {
		return originalErr
	}

	return status.Error(code, trace.UserMessage(originalErr))
}

// FromGRPC converts error from GRPC error back to trace.Error
// Debug information will be retrieved from the metadata if specified in args
func FromGRPC(err error, args ...interface{}) error {
	if err == nil {
		return nil
	}

	statusErr := status.Convert(err)
	code := statusErr.Code()
	message := statusErr.Message()

	var e error
	switch code {
	case codes.OK:
		return nil
	case codes.NotFound:
		e = &trace.NotFoundError{Message: message}
	case codes.AlreadyExists:
		e = &trace.AlreadyExistsError{Message: message}
	case codes.PermissionDenied:
		e = &trace.AccessDeniedError{Message: message}
	case codes.FailedPrecondition:
		e = &trace.CompareFailedError{Message: message}
	case codes.InvalidArgument:
		e = &trace.BadParameterError{Message: message}
	case codes.ResourceExhausted:
		e = &trace.LimitExceededError{Message: message}
	case codes.Unavailable:
		e = &trace.ConnectionProblemError{Message: message}
	case codes.Unimplemented:
		e = &trace.NotImplementedError{Message: message}
	default:
		e = err
	}
	if len(args) != 0 {
		if meta, ok := args[0].(metadata.MD); ok {
			e = DecodeDebugInfo(e, meta)
			// We return here because if it's a trace.Error then
			// frames was already extracted from metadata so
			// there's no need to capture frames once again.
			if _, ok := e.(trace.Error); ok {
				return e
			}
		}
	}
	traces := internal.CaptureTraces(1)
	return &trace.TraceErr{Err: e, Traces: traces}
}

// SetDebugInfo adds debug metadata about error (traces, original error)
// to request metadata as encoded property
func SetDebugInfo(err error, meta metadata.MD) {
	if _, ok := err.(*trace.TraceErr); !ok {
		return
	}
	out, err := json.Marshal(err)
	if err != nil {
		return
	}
	meta[DebugReportMetadata] = []string{
		base64.StdEncoding.EncodeToString(out),
	}
}

// DecodeDebugInfo decodes debug information about error
// from the metadata and returns error with enriched metadata about it
func DecodeDebugInfo(err error, meta metadata.MD) error {
	if len(meta) == 0 {
		return err
	}
	encoded, ok := meta[DebugReportMetadata]
	if !ok || len(encoded) != 1 {
		return err
	}
	data, decodeErr := base64.StdEncoding.DecodeString(encoded[0])
	if decodeErr != nil {
		return err
	}
	var raw trace.RawTrace
	if unmarshalErr := json.Unmarshal(data, &raw); unmarshalErr != nil {
		return err
	}
	if len(raw.Traces) != 0 && len(raw.Err) != 0 {
		return &trace.TraceErr{Traces: raw.Traces, Err: err, Message: raw.Message}
	}
	return err
}
