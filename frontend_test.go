// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2022 Dell Inc, or its subsidiaries.
// Copyright (c) 2022 NVIDIA CORPORATION & AFFILIATES. All rights reserved.

package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"

	pc "github.com/opiproject/opi-api/common/v1/gen/go"
	pb "github.com/opiproject/opi-api/storage/v1alpha1/gen/go"
)

func dialer() func(context.Context, string) (net.Conn, error) {
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	pb.RegisterFrontendNvmeServiceServer(server, &PluginFrontendNvme)

	go func() {
		if err := server.Serve(listener); err != nil {
			log.Fatal(err)
		}
	}()

	return func(context.Context, string) (net.Conn, error) {
		return listener.Dial()
	}
}

func spdkMockServer(l net.Listener, toSend []string) {
	for _, spdk := range toSend {
		fd, err := l.Accept()
		if err != nil {
			log.Fatal("accept error:", err)
		}
		log.Printf("SPDK mockup server: client connected [%s]", fd.RemoteAddr().Network())
		log.Printf("SPDK ID [%d]", rpcID)

		buf := make([]byte, 512)
		nr, err := fd.Read(buf)
		if err != nil {
			return
		}

		data := buf[0:nr]
		if strings.Contains(spdk, "%") {
			spdk = fmt.Sprintf(spdk, rpcID)
		}

		log.Printf("SPDK mockup server: got : %s", string(data))
		log.Printf("SPDK mockup server: snd : %s", spdk)

		_, err = fd.Write([]byte(spdk))
		if err != nil {
			log.Fatal("Write: ", err)
		}
		err = fd.(*net.UnixConn).CloseWrite()
		if err != nil {
			log.Fatal(err)
		}
	}
}

func startSpdkMockupServer() net.Listener {
	// start SPDK mockup server
	if err := os.RemoveAll(*rpcSock); err != nil {
		log.Fatal(err)
	}
	ln, err := net.Listen("unix", *rpcSock)
	if err != nil {
		log.Fatal("listen error:", err)
	}
	return ln
}

func startGrpcMockupServer() (context.Context, *grpc.ClientConn) {
	// start GRPC mockup Server
	ctx := context.Background()
	conn, err := grpc.DialContext(ctx, "", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(dialer()))
	if err != nil {
		log.Fatal(err)
	}
	return ctx, conn
}

func TestFrontEnd_CreateNVMeSubsystem(t *testing.T) {
	spec := &pb.NVMeSubsystemSpec{
		Id:           &pc.ObjectKey{Value: "subsystem-test"},
		Nqn:          "nqn.2022-09.io.spdk:opi3",
		SerialNumber: "OpiSerialNumber",
		ModelNumber:  "OpiModelNumber",
	}
	tests := []struct {
		name    string
		in      *pb.NVMeSubsystem
		out     *pb.NVMeSubsystem
		spdk    []string
		errCode codes.Code
		errMsg  string
		start   bool
	}{
		{
			"valid request with invalid SPDK response",
			&pb.NVMeSubsystem{
				Spec: spec,
			},
			nil,
			[]string{`{"id":%d,"error":{"code":0,"message":""},"result":false}`},
			codes.InvalidArgument,
			fmt.Sprintf("Could not create NQN: %v", "nqn.2022-09.io.spdk:opi3"),
			true,
		},
		{
			"valid request with empty SPDK response",
			&pb.NVMeSubsystem{
				Spec: spec,
			},
			nil,
			[]string{""},
			codes.Unknown,
			fmt.Sprintf("subsystem_nvme_create: %v", "EOF"),
			true,
		},
		{
			"valid request with ID mismatch SPDK response",
			&pb.NVMeSubsystem{
				Spec: spec,
			},
			nil,
			[]string{`{"id":0,"error":{"code":0,"message":""},"result":false}`},
			codes.Unknown,
			fmt.Sprintf("subsystem_nvme_create: %v", "json response ID mismatch"),
			true,
		},
		{
			"valid request with error code from SPDK response",
			&pb.NVMeSubsystem{
				Spec: spec,
			},
			nil,
			[]string{`{"id":%d,"error":{"code":1,"message":"myopierr"},"result":false}`},
			codes.Unknown,
			fmt.Sprintf("subsystem_nvme_create: %v", "json response error: myopierr"),
			true,
		},
		{
			"valid request with valid SPDK response",
			&pb.NVMeSubsystem{
				Spec: spec,
			},
			&pb.NVMeSubsystem{
				Spec: &pb.NVMeSubsystemSpec{
					Id:           &pc.ObjectKey{Value: "subsystem-test"},
					Nqn:          "nqn.2022-09.io.spdk:opi3",
					SerialNumber: "OpiSerialNumber",
					ModelNumber:  "OpiModelNumber",
				},
				Status: &pb.NVMeSubsystemStatus{
					FirmwareRevision: "SPDK v20.10",
				},
			},
			[]string{`{"id":%d,"error":{"code":0,"message":""},"result":true}`,
				`{"jsonrpc":"2.0","id":%d,"result":{"version":"SPDK v20.10","fields":{"major":20,"minor":10,"patch":0,"suffix":""}}}`},
			codes.OK,
			"",
			true,
		},
	}

	ctx, conn := startGrpcMockupServer()
	defer func(conn *grpc.ClientConn) {
		err := conn.Close()
		if err != nil {
			log.Fatal(err)
		}
	}(conn)
	client := pb.NewFrontendNvmeServiceClient(conn)

	ln := startSpdkMockupServer()
	defer func(ln net.Listener) {
		err := ln.Close()
		if err != nil {
			log.Fatal(err)
		}
	}(ln)

	// run tests
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.start {
				go spdkMockServer(ln, tt.spdk)
			}
			request := &pb.CreateNVMeSubsystemRequest{NvMeSubsystem: tt.in}
			response, err := client.CreateNVMeSubsystem(ctx, request)
			if response != nil {
				if !reflect.DeepEqual(response.Spec, tt.out.Spec) {
					t.Error("response: expected", tt.out.GetSpec(), "received", response.GetSpec())
				}
				if !reflect.DeepEqual(response.Status, tt.out.Status) {
					t.Error("response: expected", tt.out.GetStatus(), "received", response.GetStatus())
				}
			}

			if err != nil {
				if er, ok := status.FromError(err); ok {
					if er.Code() != tt.errCode {
						t.Error("error code: expected", codes.InvalidArgument, "received", er.Code())
					}
					if er.Message() != tt.errMsg {
						t.Error("error message: expected", tt.errMsg, "received", er.Message())
					}
				}
			}
		})
	}
}

func TestFrontEnd_UpdateNVMeSubsystem(t *testing.T) {
	tests := []struct {
		name    string
		in      *pb.NVMeSubsystem
		out     *pb.NVMeSubsystem
		errCode codes.Code
		errMsg  string
		start   bool
	}{
		{
			"unimplemented method",
			&pb.NVMeSubsystem{},
			nil,
			codes.Unimplemented,
			fmt.Sprintf("%v method is not implemented", "UpdateNVMeSubsystem"),
			false,
		},
	}

	ctx, conn := startGrpcMockupServer()
	defer func(conn *grpc.ClientConn) {
		err := conn.Close()
		if err != nil {
			log.Fatal(err)
		}
	}(conn)
	client := pb.NewFrontendNvmeServiceClient(conn)

	// run tests
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := &pb.UpdateNVMeSubsystemRequest{NvMeSubsystem: tt.in}
			response, err := client.UpdateNVMeSubsystem(ctx, request)
			if response != nil {
				t.Error("response: expected", codes.Unimplemented, "received", response)
			}

			if err != nil {
				if er, ok := status.FromError(err); ok {
					if er.Code() != tt.errCode {
						t.Error("error code: expected", codes.InvalidArgument, "received", er.Code())
					}
					if er.Message() != tt.errMsg {
						t.Error("error message: expected", tt.errMsg, "received", er.Message())
					}
				}
			}
		})
	}
}

func TestFrontEnd_ListNVMeSubsystem(t *testing.T) {
	tests := []struct {
		name    string
		out     []*pb.NVMeSubsystem
		spdk    []string
		errCode codes.Code
		errMsg  string
		start   bool
	}{
		{
			"valid request with invalid SPDK response",
			nil,
			[]string{`{"id":%d,"error":{"code":0,"message":""},"result":[]}`},
			codes.InvalidArgument,
			fmt.Sprintf("Could not create NQN: %v", "nqn.2022-09.io.spdk:opi3"),
			true,
		},
		{
			"valid request with empty SPDK response",
			nil,
			[]string{""},
			codes.Unknown,
			fmt.Sprintf("subsystem_nvme_list: %v", "EOF"),
			true,
		},
		{
			"valid request with ID mismatch SPDK response",
			nil,
			[]string{`{"id":0,"error":{"code":0,"message":""},"result":[]}`},
			codes.Unknown,
			fmt.Sprintf("subsystem_nvme_list: %v", "json response ID mismatch"),
			true,
		},
		{
			"valid request with error code from SPDK response",
			nil,
			[]string{`{"id":%d,"error":{"code":1,"message":"myopierr"},"result":[]}`},
			codes.Unknown,
			fmt.Sprintf("subsystem_nvme_list: %v", "json response error: myopierr"),
			true,
		},
		{
			"valid request with valid SPDK response",
			[]*pb.NVMeSubsystem{
				{
					Spec: &pb.NVMeSubsystemSpec{
						Nqn:          "nqn.2022-09.io.spdk:opi1",
						SerialNumber: "OpiSerialNumber1",
						ModelNumber:  "OpiModelNumber1",
					},
				},
				{
					Spec: &pb.NVMeSubsystemSpec{
						Nqn:          "nqn.2022-09.io.spdk:opi2",
						SerialNumber: "OpiSerialNumber2",
						ModelNumber:  "OpiModelNumber2",
					},
				},
				{
					Spec: &pb.NVMeSubsystemSpec{
						Nqn:          "nqn.2022-09.io.spdk:opi3",
						SerialNumber: "OpiSerialNumber3",
						ModelNumber:  "OpiModelNumber3",
					},
				},
			},
			// {'jsonrpc': '2.0', 'id': 1, 'result': [{'nqn': 'nqn.2020-12.mlnx.snap', 'serial_number': 'Mellanox_NVMe_SNAP', 'model_number': 'Mellanox NVMe SNAP Controller', 'controllers': [{'name': 'NvmeEmu0pf1', 'cntlid': 0, 'pci_bdf': 'ca:00.3', 'pci_index': 1}]}]}
			[]string{`{"id":%d,"error":{"code":0,"message":""},"result":[{"nqn": "nqn.2022-09.io.spdk:opi1", "serial_number": "OpiSerialNumber1", "model_number": "OpiModelNumber1"},{"nqn": "nqn.2022-09.io.spdk:opi2", "serial_number": "OpiSerialNumber2", "model_number": "OpiModelNumber2"},{"nqn": "nqn.2022-09.io.spdk:opi3", "serial_number": "OpiSerialNumber3", "model_number": "OpiModelNumber3"}]}`},
			codes.OK,
			"",
			true,
		},
	}

	ctx, conn := startGrpcMockupServer()
	defer func(conn *grpc.ClientConn) {
		err := conn.Close()
		if err != nil {
			log.Fatal(err)
		}
	}(conn)
	client := pb.NewFrontendNvmeServiceClient(conn)

	ln := startSpdkMockupServer()
	defer func(ln net.Listener) {
		err := ln.Close()
		if err != nil {
			log.Fatal(err)
		}
	}(ln)

	// run tests
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.start {
				go spdkMockServer(ln, tt.spdk)
			}
			request := &pb.ListNVMeSubsystemsRequest{}
			response, err := client.ListNVMeSubsystems(ctx, request)
			if response != nil {
				if !reflect.DeepEqual(response.NvMeSubsystems, tt.out) {
					t.Error("response: expected", tt.out, "received", response.NvMeSubsystems)
				}
			}

			if err != nil {
				if er, ok := status.FromError(err); ok {
					if er.Code() != tt.errCode {
						t.Error("error code: expected", codes.InvalidArgument, "received", er.Code())
					}
					if er.Message() != tt.errMsg {
						t.Error("error message: expected", tt.errMsg, "received", er.Message())
					}
				}
			}
		})
	}
}

func TestFrontEnd_GetNVMeSubsystem(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		out     *pb.NVMeSubsystem
		spdk    []string
		errCode codes.Code
		errMsg  string
		start   bool
	}{
		{
			"valid request with invalid SPDK response",
			"subsystem-test",
			nil,
			[]string{`{"id":%d,"error":{"code":0,"message":""},"result":[]}`},
			codes.InvalidArgument,
			fmt.Sprintf("Could not find NQN: %v", "nqn.2022-09.io.spdk:opi3"),
			true,
		},
		{
			"valid request with empty SPDK response",
			"subsystem-test",
			nil,
			[]string{""},
			codes.Unknown,
			fmt.Sprintf("subsystem_nvme_list: %v", "EOF"),
			true,
		},
		{
			"valid request with ID mismatch SPDK response",
			"subsystem-test",
			nil,
			[]string{`{"id":0,"error":{"code":0,"message":""},"result":[]}`},
			codes.Unknown,
			fmt.Sprintf("subsystem_nvme_list: %v", "json response ID mismatch"),
			true,
		},
		{
			"valid request with error code from SPDK response",
			"subsystem-test",
			nil,
			[]string{`{"id":%d,"error":{"code":1,"message":"myopierr"},"result":[]}`},
			codes.Unknown,
			fmt.Sprintf("subsystem_nvme_list: %v", "json response error: myopierr"),
			true,
		},
		{
			"valid request with valid SPDK response",
			"subsystem-test",
			&pb.NVMeSubsystem{
				Spec: &pb.NVMeSubsystemSpec{
					Nqn:          "nqn.2022-09.io.spdk:opi3",
					SerialNumber: "OpiSerialNumber3",
					ModelNumber:  "OpiModelNumber3",
				},
				Status: &pb.NVMeSubsystemStatus{
					FirmwareRevision: "TBD",
				},
			},
			// {'jsonrpc': '2.0', 'id': 1, 'result': [{'nqn': 'nqn.2020-12.mlnx.snap', 'serial_number': 'Mellanox_NVMe_SNAP', 'model_number': 'Mellanox NVMe SNAP Controller', 'controllers': [{'name': 'NvmeEmu0pf1', 'cntlid': 0, 'pci_bdf': 'ca:00.3', 'pci_index': 1}]}]}
			[]string{`{"id":%d,"error":{"code":0,"message":""},"result":[{"nqn": "nqn.2022-09.io.spdk:opi1", "serial_number": "OpiSerialNumber1", "model_number": "OpiModelNumber1"},{"nqn": "nqn.2022-09.io.spdk:opi2", "serial_number": "OpiSerialNumber2", "model_number": "OpiModelNumber2"},{"nqn": "nqn.2022-09.io.spdk:opi3", "serial_number": "OpiSerialNumber3", "model_number": "OpiModelNumber3"}]}`},
			codes.OK,
			"",
			true,
		},
		{
			"valid request with unknown key",
			"unknown-subsystem-id",
			nil,
			[]string{""},
			codes.Unknown,
			fmt.Sprintf("unable to find key %v", "unknown-subsystem-id"),
			false,
		},
	}

	ctx, conn := startGrpcMockupServer()
	defer func(conn *grpc.ClientConn) {
		err := conn.Close()
		if err != nil {
			log.Fatal(err)
		}
	}(conn)
	client := pb.NewFrontendNvmeServiceClient(conn)

	ln := startSpdkMockupServer()
	defer func(ln net.Listener) {
		err := ln.Close()
		if err != nil {
			log.Fatal(err)
		}
	}(ln)

	// run tests
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.start {
				go spdkMockServer(ln, tt.spdk)
			}
			request := &pb.GetNVMeSubsystemRequest{Name: tt.in}
			response, err := client.GetNVMeSubsystem(ctx, request)
			if response != nil {
				if !reflect.DeepEqual(response.Spec, tt.out.Spec) {
					t.Error("response: expected", tt.out.GetSpec(), "received", response.GetSpec())
				}
				if !reflect.DeepEqual(response.Status, tt.out.Status) {
					t.Error("response: expected", tt.out.GetStatus(), "received", response.GetStatus())
				}
			}

			if err != nil {
				if er, ok := status.FromError(err); ok {
					if er.Code() != tt.errCode {
						t.Error("error code: expected", codes.InvalidArgument, "received", er.Code())
					}
					if er.Message() != tt.errMsg {
						t.Error("error message: expected", tt.errMsg, "received", er.Message())
					}
				}
			}
		})
	}
}

func TestFrontEnd_NVMeSubsystemStats(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		out     *pb.NVMeSubsystemStatsResponse
		errCode codes.Code
		errMsg  string
		start   bool
	}{
		{
			"unimplemented method",
			"subsystem-test",
			nil,
			codes.Unimplemented,
			fmt.Sprintf("%v method is not implemented", "UpdateNVMeSubsystem"),
			false,
		},
	}

	ctx, conn := startGrpcMockupServer()
	defer func(conn *grpc.ClientConn) {
		err := conn.Close()
		if err != nil {
			log.Fatal(err)
		}
	}(conn)
	client := pb.NewFrontendNvmeServiceClient(conn)

	// run tests
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := &pb.NVMeSubsystemStatsRequest{SubsystemId: &pc.ObjectKey{Value: tt.in}}
			response, err := client.NVMeSubsystemStats(ctx, request)
			if response != nil {
				t.Error("response: expected", codes.Unimplemented, "received", response)
			}

			if err != nil {
				if er, ok := status.FromError(err); ok {
					if er.Code() != tt.errCode {
						t.Error("error code: expected", codes.InvalidArgument, "received", er.Code())
					}
					if er.Message() != tt.errMsg {
						t.Error("error message: expected", tt.errMsg, "received", er.Message())
					}
				}
			}
		})
	}
}

func TestFrontEnd_CreateNVMeController(t *testing.T) {
	spec := &pb.NVMeControllerSpec{
		Id:               &pc.ObjectKey{Value: "controller-test"},
		SubsystemId:      &pc.ObjectKey{Value: "subsystem-test"},
		PcieId:           &pb.PciEndpoint{PhysicalFunction: 1, VirtualFunction: 2},
		NvmeControllerId: 1,
	}
	controllerSpec := &pb.NVMeControllerSpec{
		Id:               &pc.ObjectKey{Value: "controller-test"},
		SubsystemId:      &pc.ObjectKey{Value: "subsystem-test"},
		PcieId:           &pb.PciEndpoint{PhysicalFunction: 1, VirtualFunction: 2},
		NvmeControllerId: 17,
	}
	tests := []struct {
		name    string
		in      *pb.NVMeController
		out     *pb.NVMeController
		spdk    []string
		errCode codes.Code
		errMsg  string
		start   bool
	}{
		{
			"valid request with invalid SPDK response",
			&pb.NVMeController{
				Spec: spec,
			},
			nil,
			[]string{`{"id":%d,"error":{"code":0,"message":""},"result":{"name": "NvmeEmu0pf0", "cntlid": -1}}`},
			codes.InvalidArgument,
			fmt.Sprintf("Could not create CTRL: %v", "controller-test"),
			true,
		},
		{
			"valid request with empty SPDK response",
			&pb.NVMeController{
				Spec: spec,
			},
			nil,
			[]string{""},
			codes.Unknown,
			fmt.Sprintf("controller_nvme_create: %v", "EOF"),
			true,
		},
		{
			"valid request with ID mismatch SPDK response",
			&pb.NVMeController{
				Spec: spec,
			},
			nil,
			[]string{`{"id":0,"error":{"code":0,"message":""},"result":{"name": "NvmeEmu0pf0", "cntlid": 17}}`},
			codes.Unknown,
			fmt.Sprintf("controller_nvme_create: %v", "json response ID mismatch"),
			true,
		},
		{
			"valid request with error code from SPDK response",
			&pb.NVMeController{
				Spec: spec,
			},
			nil,
			[]string{`{"id":%d,"error":{"code":-32602,"message":"Invalid parameters"}}`},
			codes.Unknown,
			fmt.Sprintf("controller_nvme_create: %v", "json response error: Invalid parameters"),
			true,
		},
		{
			"valid request with valid SPDK response",
			&pb.NVMeController{
				Spec: controllerSpec,
			},
			&pb.NVMeController{
				Spec: &pb.NVMeControllerSpec{
					Id:               &pc.ObjectKey{Value: "controller-test"},
					SubsystemId:      &pc.ObjectKey{Value: "subsystem-test"},
					PcieId:           &pb.PciEndpoint{PhysicalFunction: 1, VirtualFunction: 2},
					NvmeControllerId: 17,
				},
				Status: &pb.NVMeControllerStatus{
					Active: true,
				},
			},
			[]string{`{"id":%d,"error":{"code":0,"message":""},"result":{"name": "NvmeEmu0pf0", "cntlid": 17}}`},
			codes.OK,
			"",
			true,
		},
	}

	ctx, conn := startGrpcMockupServer()
	defer func(conn *grpc.ClientConn) {
		err := conn.Close()
		if err != nil {
			log.Fatal(err)
		}
	}(conn)
	client := pb.NewFrontendNvmeServiceClient(conn)

	ln := startSpdkMockupServer()
	defer func(ln net.Listener) {
		err := ln.Close()
		if err != nil {
			log.Fatal(err)
		}
	}(ln)

	// run tests
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.start {
				go spdkMockServer(ln, tt.spdk)
			}
			request := &pb.CreateNVMeControllerRequest{NvMeController: tt.in}
			response, err := client.CreateNVMeController(ctx, request)
			if response != nil {
				if !reflect.DeepEqual(response.Spec, tt.out.Spec) {
					t.Error("response: expected", tt.out.GetSpec(), "received", response.GetSpec())
				}
				if !reflect.DeepEqual(response.Status, tt.out.Status) {
					t.Error("response: expected", tt.out.GetStatus(), "received", response.GetStatus())
				}
			}

			if err != nil {
				if er, ok := status.FromError(err); ok {
					if er.Code() != tt.errCode {
						t.Error("error code: expected", codes.InvalidArgument, "received", er.Code())
					}
					if er.Message() != tt.errMsg {
						t.Error("error message: expected", tt.errMsg, "received", er.Message())
					}
				}
			}
		})
	}
}

func TestFrontEnd_UpdateNVMeController(t *testing.T) {
	tests := []struct {
		name    string
		in      *pb.NVMeController
		out     *pb.NVMeController
		errCode codes.Code
		errMsg  string
		start   bool
	}{
		{
			"unimplemented method",
			&pb.NVMeController{},
			nil,
			codes.Unimplemented,
			fmt.Sprintf("%v method is not implemented", "UpdateNVMeController"),
			false,
		},
	}

	ctx, conn := startGrpcMockupServer()
	defer func(conn *grpc.ClientConn) {
		err := conn.Close()
		if err != nil {
			log.Fatal(err)
		}
	}(conn)
	client := pb.NewFrontendNvmeServiceClient(conn)

	// run tests
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := &pb.UpdateNVMeControllerRequest{NvMeController: tt.in}
			response, err := client.UpdateNVMeController(ctx, request)
			if response != nil {
				t.Error("response: expected", codes.Unimplemented, "received", response)
			}

			if err != nil {
				if er, ok := status.FromError(err); ok {
					if er.Code() != tt.errCode {
						t.Error("error code: expected", codes.InvalidArgument, "received", er.Code())
					}
					if er.Message() != tt.errMsg {
						t.Error("error message: expected", tt.errMsg, "received", er.Message())
					}
				}
			}
		})
	}
}

func TestFrontEnd_ListNVMeControllers(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		out     []*pb.NVMeController
		spdk    []string
		errCode codes.Code
		errMsg  string
		start   bool
	}{
		{
			"valid request with invalid SPDK response",
			"subsystem-test",
			nil,
			[]string{`{"id":%d,"error":{"code":0,"message":""},"result":[]}`},
			codes.InvalidArgument,
			fmt.Sprintf("Could not create NQN: %v", "nqn.2022-09.io.spdk:opi3"),
			true,
		},
		{
			"valid request with empty SPDK response",
			"subsystem-test",
			nil,
			[]string{""},
			codes.Unknown,
			fmt.Sprintf("controller_list: %v", "EOF"),
			true,
		},
		{
			"valid request with ID mismatch SPDK response",
			"subsystem-test",
			nil,
			[]string{`{"id":0,"error":{"code":0,"message":""},"result":[]}`},
			codes.Unknown,
			fmt.Sprintf("controller_list: %v", "json response ID mismatch"),
			true,
		},
		{
			"valid request with error code from SPDK response",
			"subsystem-test",
			nil,
			[]string{`{"id":%d,"error":{"code":1,"message":"myopierr"},"result":[]}`},
			codes.Unknown,
			fmt.Sprintf("controller_list: %v", "json response error: myopierr"),
			true,
		},
		{
			"valid request with valid SPDK response",
			"subsystem-test",
			[]*pb.NVMeController{
				{
					Spec: &pb.NVMeControllerSpec{
						NvmeControllerId: 1,
					},
				},
				{
					Spec: &pb.NVMeControllerSpec{
						NvmeControllerId: 2,
					},
				},
				{
					Spec: &pb.NVMeControllerSpec{
						NvmeControllerId: 3,
					},
				},
			},
			[]string{`{"id":%d,"error":{"code":0,"message":""},"result":[{"subnqn": "nqn.2022-09.io.spdk:opi3", "cntlid": 1, "name": "NvmeEmu0pf1", "type": "nvme", "pci_index": 1, "pci_bdf": "ca:00.3"},{"subnqn": "nqn.2022-09.io.spdk:opi3", "cntlid": 2, "name": "NvmeEmu0pf1", "type": "nvme", "pci_index": 2, "pci_bdf": "ca:00.4"},{"subnqn": "nqn.2022-09.io.spdk:opi3", "cntlid": 3, "name": "NvmeEmu0pf1", "type": "nvme", "pci_index": 3, "pci_bdf": "ca:00.5"}]}`},
			codes.OK,
			"",
			true,
		},
		{
			"valid request with unknown key",
			"unknown-subsystem-id",
			nil,
			[]string{""},
			codes.Unknown,
			fmt.Sprintf("unable to find key %v", "unknown-subsystem-id"),
			false,
		},
	}

	ctx, conn := startGrpcMockupServer()
	defer func(conn *grpc.ClientConn) {
		err := conn.Close()
		if err != nil {
			log.Fatal(err)
		}
	}(conn)
	client := pb.NewFrontendNvmeServiceClient(conn)

	ln := startSpdkMockupServer()
	defer func(ln net.Listener) {
		err := ln.Close()
		if err != nil {
			log.Fatal(err)
		}
	}(ln)

	// run tests
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.start {
				go spdkMockServer(ln, tt.spdk)
			}
			request := &pb.ListNVMeControllersRequest{Parent: tt.in}
			response, err := client.ListNVMeControllers(ctx, request)
			if response != nil {
				if !reflect.DeepEqual(response.NvMeControllers, tt.out) {
					t.Error("response: expected", tt.out, "received", response.NvMeControllers)
				}
			}

			if err != nil {
				if er, ok := status.FromError(err); ok {
					if er.Code() != tt.errCode {
						t.Error("error code: expected", codes.InvalidArgument, "received", er.Code())
					}
					if er.Message() != tt.errMsg {
						t.Error("error message: expected", tt.errMsg, "received", er.Message())
					}
				}
			}
		})
	}
}

func TestFrontEnd_GetNVMeController(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		out     *pb.NVMeController
		spdk    []string
		errCode codes.Code
		errMsg  string
		start   bool
	}{
		{
			"valid request with invalid SPDK response",
			"controller-test",
			nil,
			[]string{`{"id":%d,"error":{"code":0,"message":""},"result":[]}`},
			codes.InvalidArgument,
			fmt.Sprintf("Could not find NvmeControllerId: %v", "17"),
			true,
		},
		{
			"valid request with empty SPDK response",
			"controller-test",
			nil,
			[]string{""},
			codes.Unknown,
			fmt.Sprintf("controller_list: %v", "EOF"),
			true,
		},
		{
			"valid request with ID mismatch SPDK response",
			"controller-test",
			nil,
			[]string{`{"id":0,"error":{"code":0,"message":""},"result":[]}`},
			codes.Unknown,
			fmt.Sprintf("controller_list: %v", "json response ID mismatch"),
			true,
		},
		{
			"valid request with error code from SPDK response",
			"controller-test",
			nil,
			[]string{`{"id":%d,"error":{"code":1,"message":"myopierr"},"result":[]}`},
			codes.Unknown,
			fmt.Sprintf("controller_list: %v", "json response error: myopierr"),
			true,
		},
		{
			"valid request with valid SPDK response",
			"controller-test",
			&pb.NVMeController{
				Spec: &pb.NVMeControllerSpec{
					NvmeControllerId: 17,
				},
				Status: &pb.NVMeControllerStatus{
					Active: true,
				},
			},
			[]string{`{"id":%d,"error":{"code":0,"message":""},"result":[{"subnqn": "nqn.2022-09.io.spdk:opi3", "cntlid": 1, "name": "NvmeEmu0pf1", "type": "nvme", "pci_index": 1, "pci_bdf": "ca:00.3"},{"subnqn": "nqn.2022-09.io.spdk:opi3", "cntlid": 17, "name": "NvmeEmu0pf1", "type": "nvme", "pci_index": 2, "pci_bdf": "ca:00.4"},{"subnqn": "nqn.2022-09.io.spdk:opi3", "cntlid": 3, "name": "NvmeEmu0pf1", "type": "nvme", "pci_index": 3, "pci_bdf": "ca:00.5"}]}`},
			codes.OK,
			"",
			true,
		},
		{
			"valid request with unknown key",
			"unknown-subsystem-id",
			nil,
			[]string{""},
			codes.Unknown,
			fmt.Sprintf("unable to find key %v", "unknown-subsystem-id"),
			false,
		},
	}

	ctx, conn := startGrpcMockupServer()
	defer func(conn *grpc.ClientConn) {
		err := conn.Close()
		if err != nil {
			log.Fatal(err)
		}
	}(conn)
	client := pb.NewFrontendNvmeServiceClient(conn)

	ln := startSpdkMockupServer()
	defer func(ln net.Listener) {
		err := ln.Close()
		if err != nil {
			log.Fatal(err)
		}
	}(ln)

	// run tests
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.start {
				go spdkMockServer(ln, tt.spdk)
			}
			request := &pb.GetNVMeControllerRequest{Name: tt.in}
			response, err := client.GetNVMeController(ctx, request)
			if response != nil {
				if !reflect.DeepEqual(response.Spec, tt.out.Spec) {
					t.Error("response: expected", tt.out.GetSpec(), "received", response.GetSpec())
				}
				if !reflect.DeepEqual(response.Status, tt.out.Status) {
					t.Error("response: expected", tt.out.GetStatus(), "received", response.GetStatus())
				}
			}

			if err != nil {
				if er, ok := status.FromError(err); ok {
					if er.Code() != tt.errCode {
						t.Error("error code: expected", codes.InvalidArgument, "received", er.Code())
					}
					if er.Message() != tt.errMsg {
						t.Error("error message: expected", tt.errMsg, "received", er.Message())
					}
				}
			}
		})
	}
}

func TestFrontEnd_NVMeControllerStats(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		out     *pb.NVMeControllerStatsResponse
		errCode codes.Code
		errMsg  string
		start   bool
	}{
		{
			"unimplemented method",
			"controller-test",
			nil,
			codes.Unimplemented,
			fmt.Sprintf("%v method is not implemented", "NVMeControllerStats"),
			false,
		},
	}

	ctx, conn := startGrpcMockupServer()
	defer func(conn *grpc.ClientConn) {
		err := conn.Close()
		if err != nil {
			log.Fatal(err)
		}
	}(conn)
	client := pb.NewFrontendNvmeServiceClient(conn)

	// run tests
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := &pb.NVMeControllerStatsRequest{Id: &pc.ObjectKey{Value: tt.in}}
			response, err := client.NVMeControllerStats(ctx, request)
			if response != nil {
				t.Error("response: expected", codes.Unimplemented, "received", response)
			}

			if err != nil {
				if er, ok := status.FromError(err); ok {
					if er.Code() != tt.errCode {
						t.Error("error code: expected", codes.InvalidArgument, "received", er.Code())
					}
					if er.Message() != tt.errMsg {
						t.Error("error message: expected", tt.errMsg, "received", er.Message())
					}
				}
			}
		})
	}
}

func TestFrontEnd_CreateNVMeNamespace(t *testing.T) {
	spec := &pb.NVMeNamespaceSpec{
		Id:          &pc.ObjectKey{Value: "namespace-test"},
		SubsystemId: &pc.ObjectKey{Value: "subsystem-test"},
		HostNsid:    0,
		VolumeId:    &pc.ObjectKey{Value: "Malloc1"},
		Uuid:        &pc.Uuid{Value: "1b4e28ba-2fa1-11d2-883f-b9a761bde3fb"},
		Nguid:       "1b4e28ba-2fa1-11d2-883f-b9a761bde3fb",
		Eui64:       1967554867335598546,
	}
	namespaceSpec := &pb.NVMeNamespaceSpec{
		Id:          &pc.ObjectKey{Value: "namespace-test"},
		SubsystemId: &pc.ObjectKey{Value: "subsystem-test"},
		HostNsid:    22,
		VolumeId:    &pc.ObjectKey{Value: "Malloc1"},
		Uuid:        &pc.Uuid{Value: "1b4e28ba-2fa1-11d2-883f-b9a761bde3fb"},
		Nguid:       "1b4e28ba-2fa1-11d2-883f-b9a761bde3fb",
		Eui64:       1967554867335598546,
	}
	tests := []struct {
		name    string
		in      *pb.NVMeNamespace
		out     *pb.NVMeNamespace
		spdk    []string
		errCode codes.Code
		errMsg  string
		start   bool
	}{
		{
			"valid request with invalid SPDK response",
			&pb.NVMeNamespace{
				Spec: spec,
			},
			nil,
			[]string{`{"id":%d,"error":{"code":0,"message":""},"result":false}`},
			codes.InvalidArgument,
			fmt.Sprintf("Could not create NS: %v", "namespace-test"),
			true,
		},
		{
			"valid request with empty SPDK response",
			&pb.NVMeNamespace{
				Spec: spec,
			},
			nil,
			[]string{""},
			codes.Unknown,
			fmt.Sprintf("controller_nvme_namespace_attach: %v", "EOF"),
			true,
		},
		{
			"valid request with ID mismatch SPDK response",
			&pb.NVMeNamespace{
				Spec: spec,
			},
			nil,
			[]string{`{"id":0,"error":{"code":0,"message":""},"result":false}`},
			codes.Unknown,
			fmt.Sprintf("controller_nvme_namespace_attach: %v", "json response ID mismatch"),
			true,
		},
		{
			"valid request with error code from SPDK response",
			&pb.NVMeNamespace{
				Spec: spec,
			},
			nil,
			[]string{`{"id":%d,"error":{"code":1,"message":"myopierr"},"result":false}`},
			codes.Unknown,
			fmt.Sprintf("controller_nvme_namespace_attach: %v", "json response error: myopierr"),
			true,
		},
		{
			"valid request with valid SPDK response",
			&pb.NVMeNamespace{
				Spec: namespaceSpec,
			},
			&pb.NVMeNamespace{
				Spec: &pb.NVMeNamespaceSpec{
					Id:          &pc.ObjectKey{Value: "namespace-test"},
					SubsystemId: &pc.ObjectKey{Value: "subsystem-test"},
					HostNsid:    22,
					VolumeId:    &pc.ObjectKey{Value: "Malloc1"},
					Uuid:        &pc.Uuid{Value: "1b4e28ba-2fa1-11d2-883f-b9a761bde3fb"},
					Nguid:       "1b4e28ba-2fa1-11d2-883f-b9a761bde3fb",
					Eui64:       1967554867335598546,
				},
				Status: &pb.NVMeNamespaceStatus{
					PciState:     2,
					PciOperState: 1,
				},
			},
			[]string{`{"id":%d,"error":{"code":0,"message":""},"result":true}`},
			codes.OK,
			"",
			true,
		},
	}

	ctx, conn := startGrpcMockupServer()
	defer func(conn *grpc.ClientConn) {
		err := conn.Close()
		if err != nil {
			log.Fatal(err)
		}
	}(conn)
	client := pb.NewFrontendNvmeServiceClient(conn)

	ln := startSpdkMockupServer()
	defer func(ln net.Listener) {
		err := ln.Close()
		if err != nil {
			log.Fatal(err)
		}
	}(ln)

	// run tests
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.start {
				go spdkMockServer(ln, tt.spdk)
			}
			request := &pb.CreateNVMeNamespaceRequest{NvMeNamespace: tt.in}
			response, err := client.CreateNVMeNamespace(ctx, request)
			if response != nil {
				if !reflect.DeepEqual(response.Spec, tt.out.Spec) {
					t.Error("response: expected", tt.out.GetSpec(), "received", response.GetSpec())
				}
				if !reflect.DeepEqual(response.Status, tt.out.Status) {
					t.Error("response: expected", tt.out.GetStatus(), "received", response.GetStatus())
				}
			}

			if err != nil {
				if er, ok := status.FromError(err); ok {
					if er.Code() != tt.errCode {
						t.Error("error code: expected", codes.InvalidArgument, "received", er.Code())
					}
					if er.Message() != tt.errMsg {
						t.Error("error message: expected", tt.errMsg, "received", er.Message())
					}
				}
			}
		})
	}
}

func TestFrontEnd_UpdateNVMeNamespace(t *testing.T) {
	tests := []struct {
		name    string
		in      *pb.NVMeNamespace
		out     *pb.NVMeNamespace
		errCode codes.Code
		errMsg  string
		start   bool
	}{
		{
			"unimplemented method",
			&pb.NVMeNamespace{},
			nil,
			codes.Unimplemented,
			fmt.Sprintf("%v method is not implemented", "UpdateNVMeNamespace"),
			false,
		},
	}

	ctx, conn := startGrpcMockupServer()
	defer func(conn *grpc.ClientConn) {
		err := conn.Close()
		if err != nil {
			log.Fatal(err)
		}
	}(conn)
	client := pb.NewFrontendNvmeServiceClient(conn)

	// run tests
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := &pb.UpdateNVMeNamespaceRequest{NvMeNamespace: tt.in}
			response, err := client.UpdateNVMeNamespace(ctx, request)
			if response != nil {
				t.Error("response: expected", codes.Unimplemented, "received", response)
			}

			if err != nil {
				if er, ok := status.FromError(err); ok {
					if er.Code() != tt.errCode {
						t.Error("error code: expected", codes.InvalidArgument, "received", er.Code())
					}
					if er.Message() != tt.errMsg {
						t.Error("error message: expected", tt.errMsg, "received", er.Message())
					}
				}
			}
		})
	}
}

func TestFrontEnd_ListNVMeNamespaces(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		out     []*pb.NVMeNamespace
		spdk    []string
		errCode codes.Code
		errMsg  string
		start   bool
	}{
		{
			"valid request with invalid SPDK response",
			"subsystem-test",
			nil,
			[]string{`{"id":%d,"error":{"code":0,"message":""},"result":{"name":"","cntlid":0,"Namespaces":null}}`},
			codes.InvalidArgument,
			fmt.Sprintf("Could not create NQN: %v", "nqn.2022-09.io.spdk:opi3"),
			true,
		},
		{
			"valid request with invalid marshal SPDK response",
			"subsystem-test",
			nil,
			[]string{`{"id":%d,"error":{"code":0,"message":""},"result":[]}`},
			codes.Unknown,
			fmt.Sprintf("controller_nvme_namespace_list: %v", "json: cannot unmarshal array into Go struct field .result of type main.NvdaControllerNvmeNamespaceListResult"),
			true,
		},
		{
			"valid request with empty SPDK response",
			"subsystem-test",
			nil,
			[]string{""},
			codes.Unknown,
			fmt.Sprintf("controller_nvme_namespace_list: %v", "EOF"),
			true,
		},
		{
			"valid request with ID mismatch SPDK response",
			"subsystem-test",
			nil,
			[]string{`{"id":0,"error":{"code":0,"message":""},"result":{"name":"","cntlid":0,"Namespaces":null}}`},
			codes.Unknown,
			fmt.Sprintf("controller_nvme_namespace_list: %v", "json response ID mismatch"),
			true,
		},
		{
			"valid request with error code from SPDK response",
			"subsystem-test",
			nil,
			[]string{`{"id":%d,"error":{"code":1,"message":"myopierr"}}`},
			codes.Unknown,
			fmt.Sprintf("controller_nvme_namespace_list: %v", "json response error: myopierr"),
			true,
		},
		{
			"valid request with valid SPDK response",
			"subsystem-test",
			[]*pb.NVMeNamespace{
				{
					Spec: &pb.NVMeNamespaceSpec{
						HostNsid: 11,
					},
				},
				{
					Spec: &pb.NVMeNamespaceSpec{
						HostNsid: 12,
					},
				},
				{
					Spec: &pb.NVMeNamespaceSpec{
						HostNsid: 13,
					},
				},
			},
			[]string{`{"id":%d,"error":{"code":0,"message":""},"result":{"name": "NvmeEmu0pf1", "cntlid": 1, "Namespaces": [{"nsid": 11, "bdev": "Malloc0", "bdev_type": "spdk", "qn": "", "protocol": ""},{"nsid": 12, "bdev": "Malloc1", "bdev_type": "spdk", "qn": "", "protocol": ""},{"nsid": 13, "bdev": "Malloc2", "bdev_type": "spdk", "qn": "", "protocol": ""}]}}`},
			codes.OK,
			"",
			true,
		},
		{
			"valid request with unknown key",
			"unknown-namespace-id",
			nil,
			[]string{""},
			codes.Unknown,
			fmt.Sprintf("unable to find key %v", "unknown-namespace-id"),
			false,
		},
	}

	ctx, conn := startGrpcMockupServer()
	defer func(conn *grpc.ClientConn) {
		err := conn.Close()
		if err != nil {
			log.Fatal(err)
		}
	}(conn)
	client := pb.NewFrontendNvmeServiceClient(conn)

	ln := startSpdkMockupServer()
	defer func(ln net.Listener) {
		err := ln.Close()
		if err != nil {
			log.Fatal(err)
		}
	}(ln)

	// run tests
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.start {
				go spdkMockServer(ln, tt.spdk)
			}
			request := &pb.ListNVMeNamespacesRequest{Parent: tt.in}
			response, err := client.ListNVMeNamespaces(ctx, request)
			if response != nil {
				if !reflect.DeepEqual(response.NvMeNamespaces, tt.out) {
					t.Error("response: expected", tt.out, "received", response.NvMeNamespaces)
				}
			}

			if err != nil {
				if er, ok := status.FromError(err); ok {
					if er.Code() != tt.errCode {
						t.Error("error code: expected", codes.InvalidArgument, "received", er.Code())
					}
					if er.Message() != tt.errMsg {
						t.Error("error message: expected", tt.errMsg, "received", er.Message())
					}
				}
			}
		})
	}
}

func TestFrontEnd_GetNVMeNamespace(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		out     *pb.NVMeNamespace
		spdk    []string
		errCode codes.Code
		errMsg  string
		start   bool
	}{
		{
			"valid request with invalid SPDK response",
			"namespace-test",
			nil,
			[]string{`{"id":%d,"error":{"code":0,"message":""},"result":{"name":"","cntlid":17,"Namespaces":null}}`},
			codes.InvalidArgument,
			fmt.Sprintf("Could not find HostNsid: %v", "22"),
			true,
		},
		{
			"valid request with invalid marshal SPDK response",
			"namespace-test",
			nil,
			[]string{`{"id":%d,"error":{"code":0,"message":""},"result":[]}`},
			codes.Unknown,
			fmt.Sprintf("controller_nvme_namespace_list: %v", "json: cannot unmarshal array into Go struct field .result of type main.NvdaControllerNvmeNamespaceListResult"),
			true,
		},
		{
			"valid request with empty SPDK response",
			"namespace-test",
			nil,
			[]string{""},
			codes.Unknown,
			fmt.Sprintf("controller_nvme_namespace_list: %v", "EOF"),
			true,
		},
		{
			"valid request with ID mismatch SPDK response",
			"namespace-test",
			nil,
			[]string{`{"id":0,"error":{"code":0,"message":""},"result":{"name":"","cntlid":0,"Namespaces":null}}`},
			codes.Unknown,
			fmt.Sprintf("controller_nvme_namespace_list: %v", "json response ID mismatch"),
			true,
		},
		{
			"valid request with error code from SPDK response",
			"namespace-test",
			nil,
			[]string{`{"id":%d,"error":{"code":1,"message":"myopierr"}}`},
			codes.Unknown,
			fmt.Sprintf("controller_nvme_namespace_list: %v", "json response error: myopierr"),
			true,
		},
		{
			"valid request with valid SPDK response",
			"namespace-test",
			&pb.NVMeNamespace{
				Spec: &pb.NVMeNamespaceSpec{
					HostNsid: 22,
				},
				Status: &pb.NVMeNamespaceStatus{
					PciState:     2,
					PciOperState: 1,
				},
			},
			[]string{`{"id":%d,"error":{"code":0,"message":""},"result":{"name": "NvmeEmu0pf1", "cntlid": 1, "Namespaces": [{"nsid": 11, "bdev": "Malloc0", "bdev_type": "spdk", "qn": "", "protocol": ""},{"nsid": 22, "bdev": "Malloc1", "bdev_type": "spdk", "qn": "", "protocol": ""},{"nsid": 13, "bdev": "Malloc2", "bdev_type": "spdk", "qn": "", "protocol": ""}]}}`},
			codes.OK,
			"",
			true,
		},
		{
			"valid request with unknown key",
			"unknown-namespace-id",
			nil,
			[]string{""},
			codes.Unknown,
			fmt.Sprintf("unable to find key %v", "unknown-namespace-id"),
			false,
		},
	}

	ctx, conn := startGrpcMockupServer()
	defer func(conn *grpc.ClientConn) {
		err := conn.Close()
		if err != nil {
			log.Fatal(err)
		}
	}(conn)
	client := pb.NewFrontendNvmeServiceClient(conn)

	ln := startSpdkMockupServer()
	defer func(ln net.Listener) {
		err := ln.Close()
		if err != nil {
			log.Fatal(err)
		}
	}(ln)

	// run tests
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.start {
				go spdkMockServer(ln, tt.spdk)
			}
			request := &pb.GetNVMeNamespaceRequest{Name: tt.in}
			response, err := client.GetNVMeNamespace(ctx, request)
			if response != nil {
				if !reflect.DeepEqual(response.Spec, tt.out.Spec) {
					t.Error("response: expected", tt.out.GetSpec(), "received", response.GetSpec())
				}
				if !reflect.DeepEqual(response.Status, tt.out.Status) {
					t.Error("response: expected", tt.out.GetStatus(), "received", response.GetStatus())
				}
			}

			if err != nil {
				if er, ok := status.FromError(err); ok {
					if er.Code() != tt.errCode {
						t.Error("error code: expected", codes.InvalidArgument, "received", er.Code())
					}
					if er.Message() != tt.errMsg {
						t.Error("error message: expected", tt.errMsg, "received", er.Message())
					}
				}
			}
		})
	}
}

func TestFrontEnd_NVMeNamespaceStats(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		out     *pb.VolumeStats
		spdk    []string
		errCode codes.Code
		errMsg  string
		start   bool
	}{
		{
			"valid request with invalid SPDK response",
			"namespace-test",
			nil,
			[]string{`{"id":%d,"error":{"code":0,"message":""},"result":{"controllers":[{"name":"NvmeEmu0pf1","bdevs":[]}]}}`},
			codes.InvalidArgument,
			fmt.Sprintf("Could not find BdevName: %v", "Malloc1"),
			true,
		},
		{
			"valid request with invalid marshal SPDK response",
			"namespace-test",
			nil,
			[]string{`{"id":%d,"error":{"code":0,"message":""},"result":[]}`},
			codes.Unknown,
			fmt.Sprintf("controller_nvme_get_iostat: %v", "json: cannot unmarshal array into Go struct field .result of type main.NvdaControllerNvmeStatsResult"),
			true,
		},
		{
			"valid request with empty SPDK response",
			"namespace-test",
			nil,
			[]string{""},
			codes.Unknown,
			fmt.Sprintf("controller_nvme_get_iostat: %v", "EOF"),
			true,
		},
		{
			"valid request with ID mismatch SPDK response",
			"namespace-test",
			nil,
			[]string{`{"id":0,"error":{"code":0,"message":""},"result":{"controllers":[{"name":"NvmeEmu0pf1","bdevs":[]}]}}`},
			codes.Unknown,
			fmt.Sprintf("controller_nvme_get_iostat: %v", "json response ID mismatch"),
			true,
		},
		{
			"valid request with error code from SPDK response",
			"namespace-test",
			nil,
			[]string{`{"id":%d,"error":{"code":1,"message":"myopierr"}}`},
			codes.Unknown,
			fmt.Sprintf("controller_nvme_get_iostat: %v", "json response error: myopierr"),
			true,
		},
		{
			"valid request with valid SPDK response",
			"namespace-test",
			&pb.VolumeStats{
				ReadOpsCount:  12345,
				WriteOpsCount: 54321,
			},
			[]string{`{"id":%d,"error":{"code":0,"message":""},"result": {"controllers":[{"name":"NvmeEmu0pf1","bdevs":[{"bdev_name":"Malloc0","read_ios":55,"completed_read_ios":55,"write_ios":33,"completed_write_ios":33,"flush_ios":0,"completed_flush_ios":0,"err_read_ios":0,"err_write_ios":0,"err_flush_ios":0},{"bdev_name":"Malloc1","read_ios":12345,"completed_read_ios":12345,"write_ios":54321,"completed_write_ios":54321,"flush_ios":0,"completed_flush_ios":0,"err_read_ios":0,"err_write_ios":0,"err_flush_ios":0}]}]}}`},
			codes.OK,
			"",
			true,
		},
		{
			"valid request with unknown key",
			"unknown-namespace-id",
			nil,
			[]string{""},
			codes.Unknown,
			fmt.Sprintf("unable to find key %v", "unknown-namespace-id"),
			false,
		},
	}

	ctx, conn := startGrpcMockupServer()
	defer func(conn *grpc.ClientConn) {
		err := conn.Close()
		if err != nil {
			log.Fatal(err)
		}
	}(conn)
	client := pb.NewFrontendNvmeServiceClient(conn)

	ln := startSpdkMockupServer()
	defer func(ln net.Listener) {
		err := ln.Close()
		if err != nil {
			log.Fatal(err)
		}
	}(ln)

	// run tests
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.start {
				go spdkMockServer(ln, tt.spdk)
			}
			request := &pb.NVMeNamespaceStatsRequest{NamespaceId: &pc.ObjectKey{Value: tt.in}}
			response, err := client.NVMeNamespaceStats(ctx, request)
			if response != nil {
				if !reflect.DeepEqual(response.Stats, tt.out) {
					t.Error("response: expected", tt.out, "received", response.Stats)
				}
			}

			if err != nil {
				if er, ok := status.FromError(err); ok {
					if er.Code() != tt.errCode {
						t.Error("error code: expected", codes.InvalidArgument, "received", er.Code())
					}
					if er.Message() != tt.errMsg {
						t.Error("error message: expected", tt.errMsg, "received", er.Message())
					}
				}
			}
		})
	}
}

func TestFrontEnd_DeleteNVMeNamespace(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		out     *emptypb.Empty
		spdk    []string
		errCode codes.Code
		errMsg  string
		start   bool
	}{
		{
			"valid request with invalid SPDK response",
			"namespace-test",
			nil,
			[]string{`{"id":%d,"error":{"code":0,"message":""},"result":false}`},
			codes.InvalidArgument,
			fmt.Sprintf("Could not delete NS: %v", "subsystem-test"),
			true,
		},
		{
			"valid request with empty SPDK response",
			"namespace-test",
			nil,
			[]string{""},
			codes.Unknown,
			fmt.Sprintf("controller_nvme_namespace_detach: %v", "EOF"),
			true,
		},
		{
			"valid request with ID mismatch SPDK response",
			"namespace-test",
			nil,
			[]string{`{"id":0,"error":{"code":0,"message":""},"result":false}`},
			codes.Unknown,
			fmt.Sprintf("controller_nvme_namespace_detach: %v", "json response ID mismatch"),
			true,
		},
		{
			"valid request with error code from SPDK response",
			"namespace-test",
			nil,
			[]string{`{"id":%d,"error":{"code":1,"message":"myopierr"},"result":false}`},
			codes.Unknown,
			fmt.Sprintf("controller_nvme_namespace_detach: %v", "json response error: myopierr"),
			true,
		},
		{
			"valid request with valid SPDK response",
			"namespace-test",
			&emptypb.Empty{},
			[]string{`{"id":%d,"error":{"code":0,"message":""},"result":true}`}, // `{"jsonrpc": "2.0", "id": 1, "result": True}`,
			codes.OK,
			"",
			true,
		},
		{
			"valid request with unknown key",
			"unknown-namespace-id",
			nil,
			[]string{""},
			codes.Unknown,
			fmt.Sprintf("unable to find key %v", "unknown-namespace-id"),
			false,
		},
	}

	ctx, conn := startGrpcMockupServer()
	defer func(conn *grpc.ClientConn) {
		err := conn.Close()
		if err != nil {
			log.Fatal(err)
		}
	}(conn)
	client := pb.NewFrontendNvmeServiceClient(conn)

	ln := startSpdkMockupServer()
	defer func(ln net.Listener) {
		err := ln.Close()
		if err != nil {
			log.Fatal(err)
		}
	}(ln)

	// run tests
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.start {
				go spdkMockServer(ln, tt.spdk)
			}
			request := &pb.DeleteNVMeNamespaceRequest{Name: tt.in}
			response, err := client.DeleteNVMeNamespace(ctx, request)
			if err != nil {
				if er, ok := status.FromError(err); ok {
					if er.Code() != tt.errCode {
						t.Error("error code: expected", codes.InvalidArgument, "received", er.Code())
					}
					if er.Message() != tt.errMsg {
						t.Error("error message: expected", tt.errMsg, "received", er.Message())
					}
				}
			}
			if reflect.TypeOf(response) != reflect.TypeOf(tt.out) {
				t.Error("response: expected", reflect.TypeOf(tt.out), "received", reflect.TypeOf(response))
			}
		})
	}
}

func TestFrontEnd_DeleteNVMeController(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		out     *emptypb.Empty
		spdk    []string
		errCode codes.Code
		errMsg  string
		start   bool
	}{
		{
			"valid request with invalid SPDK response",
			"controller-test",
			nil,
			[]string{`{"id":%d,"error":{"code":0,"message":""},"result":false}`},
			codes.InvalidArgument,
			fmt.Sprintf("Could not delete NQN:ID %v", "nqn.2022-09.io.spdk:opi3:17"),
			true,
		},
		{
			"valid request with empty SPDK response",
			"controller-test",
			nil,
			[]string{""},
			codes.Unknown,
			fmt.Sprintf("controller_nvme_delete: %v", "EOF"),
			true,
		},
		{
			"valid request with ID mismatch SPDK response",
			"controller-test",
			nil,
			[]string{`{"id":0,"error":{"code":0,"message":""},"result":false}`},
			codes.Unknown,
			fmt.Sprintf("controller_nvme_delete: %v", "json response ID mismatch"),
			true,
		},
		{
			"valid request with error code from SPDK response",
			"controller-test",
			nil,
			[]string{`{"id":%d,"error":{"code":1,"message":"myopierr"},"result":false}`},
			codes.Unknown,
			fmt.Sprintf("controller_nvme_delete: %v", "json response error: myopierr"),
			true,
		},
		{
			"valid request with valid SPDK response",
			"controller-test",
			&emptypb.Empty{},
			[]string{`{"id":%d,"error":{"code":0,"message":""},"result":true}`}, // `{"jsonrpc": "2.0", "id": 1, "result": True}`,
			codes.OK,
			"",
			true,
		},
		{
			"valid request with unknown key",
			"unknown-controller-id",
			nil,
			[]string{""},
			codes.Unknown,
			fmt.Sprintf("error finding controller %v", "unknown-controller-id"),
			false,
		},
	}

	ctx, conn := startGrpcMockupServer()
	defer func(conn *grpc.ClientConn) {
		err := conn.Close()
		if err != nil {
			log.Fatal(err)
		}
	}(conn)
	client := pb.NewFrontendNvmeServiceClient(conn)

	ln := startSpdkMockupServer()
	defer func(ln net.Listener) {
		err := ln.Close()
		if err != nil {
			log.Fatal(err)
		}
	}(ln)

	// run tests
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.start {
				go spdkMockServer(ln, tt.spdk)
			}
			request := &pb.DeleteNVMeControllerRequest{Name: tt.in}
			response, err := client.DeleteNVMeController(ctx, request)
			if err != nil {
				if er, ok := status.FromError(err); ok {
					if er.Code() != tt.errCode {
						t.Error("error code: expected", codes.InvalidArgument, "received", er.Code())
					}
					if er.Message() != tt.errMsg {
						t.Error("error message: expected", tt.errMsg, "received", er.Message())
					}
				}
			}
			if reflect.TypeOf(response) != reflect.TypeOf(tt.out) {
				t.Error("response: expected", reflect.TypeOf(tt.out), "received", reflect.TypeOf(response))
			}
		})
	}
}

func TestFrontEnd_DeleteNVMeSubsystem(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		out     *emptypb.Empty
		spdk    []string
		errCode codes.Code
		errMsg  string
		start   bool
	}{
		{
			"valid request with invalid SPDK response",
			"subsystem-test",
			nil,
			[]string{`{"id":%d,"error":{"code":0,"message":""},"result":false}`},
			codes.InvalidArgument,
			fmt.Sprintf("Could not delete NQN: %v", "nqn.2022-09.io.spdk:opi3"),
			true,
		},
		{
			"valid request with empty SPDK response",
			"subsystem-test",
			nil,
			[]string{""},
			codes.Unknown,
			fmt.Sprintf("subsystem_nvme_delete: %v", "EOF"),
			true,
		},
		{
			"valid request with ID mismatch SPDK response",
			"subsystem-test",
			nil,
			[]string{`{"id":0,"error":{"code":0,"message":""},"result":false}`},
			codes.Unknown,
			fmt.Sprintf("subsystem_nvme_delete: %v", "json response ID mismatch"),
			true,
		},
		{
			"valid request with error code from SPDK response",
			"subsystem-test",
			nil,
			[]string{`{"id":%d,"error":{"code":1,"message":"myopierr"},"result":false}`},
			codes.Unknown,
			fmt.Sprintf("subsystem_nvme_delete: %v", "json response error: myopierr"),
			true,
		},
		{
			"valid request with valid SPDK response",
			"subsystem-test",
			&emptypb.Empty{},
			[]string{`{"id":%d,"error":{"code":0,"message":""},"result":true}`}, // `{"jsonrpc": "2.0", "id": 1, "result": True}`,
			codes.OK,
			"",
			true,
		},
		{
			"valid request with unknown key",
			"unknown-subsystem-id",
			nil,
			[]string{""},
			codes.Unknown,
			fmt.Sprintf("unable to find key %v", "unknown-subsystem-id"),
			false,
		},
	}

	ctx, conn := startGrpcMockupServer()
	defer func(conn *grpc.ClientConn) {
		err := conn.Close()
		if err != nil {
			log.Fatal(err)
		}
	}(conn)
	client := pb.NewFrontendNvmeServiceClient(conn)

	ln := startSpdkMockupServer()
	defer func(ln net.Listener) {
		err := ln.Close()
		if err != nil {
			log.Fatal(err)
		}
	}(ln)

	// run tests
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.start {
				go spdkMockServer(ln, tt.spdk)
			}
			request := &pb.DeleteNVMeSubsystemRequest{Name: tt.in}
			response, err := client.DeleteNVMeSubsystem(ctx, request)
			if err != nil {
				if er, ok := status.FromError(err); ok {
					if er.Code() != tt.errCode {
						t.Error("error code: expected", codes.InvalidArgument, "received", er.Code())
					}
					if er.Message() != tt.errMsg {
						t.Error("error message: expected", tt.errMsg, "received", er.Message())
					}
				}
			}
			if reflect.TypeOf(response) != reflect.TypeOf(tt.out) {
				t.Error("response: expected", reflect.TypeOf(tt.out), "received", reflect.TypeOf(response))
			}
		})
	}
}
