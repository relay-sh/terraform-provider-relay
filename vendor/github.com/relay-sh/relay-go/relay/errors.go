package relay

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func IsNotFound(err error) bool {
	return err != nil && status.Code(err) == codes.NotFound
}

func IsAlreadyExists(err error) bool {
	return err != nil && status.Code(err) == codes.AlreadyExists
}
