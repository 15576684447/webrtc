package sfu

import (
	"net"

	"google.golang.org/grpc"
	"webrtc/webrtc/ion-sfu/pkg/log"

	pb "webrtc/webrtc/ion-sfu/pkg/proto"
)

type server struct {
	pb.UnimplementedSFUServer
}

// InitLogLevel can be used to set a log level for the sfu.
func InitLogLevel(level string) {
	log.Init(level)
}

// Init initializes the SFU gRPC server at the configured port.
func Init(port string) {
	lis, err := net.Listen("tcp", port)
	if err != nil {
		log.Logger.Errorf("failed to listen: %v", err)
	}
	s := grpc.NewServer()
	pb.RegisterSFUServer(s, &server{})
	if err := s.Serve(lis); err != nil {
		log.Logger.Errorf("failed to serve: %v", err)
	}
}
