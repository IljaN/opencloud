package command

import (
	"context"
	"fmt"

	gatewayv1beta1 "github.com/cs3org/go-cs3apis/cs3/gateway/v1beta1"
	userpb "github.com/cs3org/go-cs3apis/cs3/identity/user/v1beta1"
	rpc "github.com/cs3org/go-cs3apis/cs3/rpc/v1beta1"
	providerv1beta1 "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"

	"google.golang.org/grpc/metadata"

	ctxpkg "github.com/opencloud-eu/reva/v2/pkg/ctx"

	"github.com/opencloud-eu/opencloud/pkg/registry"

	"github.com/opencloud-eu/reva/v2/pkg/rgrpc/todo/pool"
	"github.com/opencloud-eu/reva/v2/pkg/sharedconf"
	"io"
	"net"
	"os"

	"github.com/oklog/run"
	"github.com/opencloud-eu/opencloud/pkg/config/configlog"
	"github.com/opencloud-eu/opencloud/pkg/log"
	"github.com/opencloud-eu/opencloud/services/sftp/pkg/config"
	"github.com/opencloud-eu/opencloud/services/sftp/pkg/config/parser"
	"github.com/opencloud-eu/opencloud/services/sftp/pkg/logging"
	"github.com/pkg/sftp"
	"github.com/urfave/cli/v2"
	"golang.org/x/crypto/ssh"
)

// Server is the entry point for the server command.
func Server(cfg *config.Config) *cli.Command {
	return &cli.Command{
		Name:     "server",
		Usage:    fmt.Sprintf("start the %s service without runtime (unsupervised mode)", cfg.Service.Name),
		Category: "server",
		Before: func(c *cli.Context) error {
			return configlog.ReturnFatal(parser.ParseConfig(cfg))
		},
		Action: func(c *cli.Context) error {
			logger := logging.Configure(cfg.Service.Name, cfg.Log)

			gr := run.Group{}
			ctx, cancel := context.WithCancel(c.Context)

			defer cancel()

			gr.Add(func() error {
				// init reva shared config explicitly as the go-micro based ocdav does not use
				// the reva runtime. But we need e.g. the shared client settings to be initialized
				sc := map[string]interface{}{
					"jwt_secret":                cfg.TokenManager.JWTSecret,
					"gatewaysvc":                cfg.Reva.Address,
					"skip_user_groups_in_token": cfg.SkipUserGroupsInToken,
					"grpc_client_options":       cfg.Reva.GetGRPCClientConfig(),
				}

				if err := sharedconf.Decode(sc); err != nil {
					logger.Error().Err(err).Msg("error decoding shared config for ocdav")
				}

				cfg.GatewaySelector = "eu.opencloud.api.gateway"

				sftpSrv(ctx, cfg, logger)

				return nil

			}, func(err error) {
				if err == nil {
					logger.Info().
						Str("transport", "http").
						Str("server", cfg.Service.Name).
						Msg("Shutting down server")
				} else {
					logger.Error().Err(err).
						Str("transport", "http").
						Str("server", cfg.Service.Name).
						Msg("Shutting down server")
				}

				cancel()
			})

			return gr.Run()
		},
	}
}

func sftpSrv(ctx context.Context, cfg *config.Config, lg log.Logger) {
	debugStream := io.Discard
	if false {
		debugStream = os.Stderr
	}

	gatewaySelector, err := pool.GatewaySelector(
		cfg.Reva.Address,
		append(
			cfg.Reva.GetRevaOptions(),
			pool.WithRegistry(registry.GetRegistry()),
		)...)
	if err != nil {
		panic(err)
	}

	gwapi, err := gatewaySelector.Next()
	if err != nil {
		panic(err)
	}

	// An SSH server is represented by a ServerConfig, which holds
	// certificate details and handles authentication of ServerConns.
	config := &ssh.ServerConfig{

		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {

			fmt.Fprintf(debugStream, "Login: %s\n", c.User())
			userName := c.User()

			ctx := context.Background()
			authRes, err := gwapi.Authenticate(ctx, &gatewayv1beta1.AuthenticateRequest{
				Type:         "machine",
				ClientId:     "username:" + userName,
				ClientSecret: cfg.MachineAuthAPIKey,
			})

			if err != nil {
				lg.Debug().Err(err).Msgf("Auth failed")
				return nil, err
			}

			if authRes.GetStatus().GetCode() != rpc.Code_CODE_OK {
				authErr := fmt.Errorf(authRes.GetStatus().GetMessage())
				lg.Debug().Err(authErr).Msgf("Auth request returned Not OK")
				return nil, authErr
			}

			granteeCtx := ctxpkg.ContextSetUser(context.Background(), &userpb.User{Id: authRes.GetUser().GetId()})
			granteeCtx = metadata.AppendToOutgoingContext(granteeCtx, ctxpkg.TokenHeader, authRes.GetToken())

			// Get Home Path
			homeResp, err := gwapi.GetHome(granteeCtx, &providerv1beta1.GetHomeRequest{})
			if err != nil {
				panic(err)
			}

			// List Spaces
			storageSpacesList, err := gwapi.ListStorageSpaces(granteeCtx, &providerv1beta1.ListStorageSpacesRequest{
				PageSize:  0,
				PageToken: "",
			})

			if err != nil {
				panic(err)
			}

			fmt.Println(homeResp.GetPath())
			fmt.Println(storageSpacesList.String())

			return nil, fmt.Errorf("password rejected for %q", c.User())
		},
	}

	privateBytes, err := os.ReadFile(cfg.ServerCertPath)
	if err != nil {
		lg.Fatal().Err(err).Msg("Failed to load private key")
	}

	private, err := ssh.ParsePrivateKey(privateBytes)
	if err != nil {
		lg.Fatal().Err(err).Msg("Failed to parse private key")
	}

	config.AddHostKey(private)

	// Once a ServerConfig has been configured, connections can be
	// accepted.
	listener, err := net.Listen("tcp", "127.0.0.1:2022")
	if err != nil {
		lg.Fatal().Err(err).Msg("failed to listen for connection")
	}
	fmt.Printf("Listening on %v\n", listener.Addr())

	nConn, err := listener.Accept()
	if err != nil {
		lg.Fatal().Err(err).Msg("failed to accept incoming connection")
	}

	// Before use, a handshake must be performed on the incoming net.Conn.
	sconn, chans, reqs, err := ssh.NewServerConn(nConn, config)
	if err != nil {
		lg.Fatal().Err(err).Msg("failed to handshake")
	}
	lg.Info().Msgf("login detected: %s", sconn.User())
	fmt.Fprintf(debugStream, "SSH server established\n")

	// The incoming Request channel must be serviced.
	go ssh.DiscardRequests(reqs)

	// Service the incoming Channel channel.
	for newChannel := range chans {
		// Channels have a type, depending on the application level
		// protocol intended. In the case of an SFTP session, this is "subsystem"
		// with a payload string of "<length=4>sftp"
		fmt.Fprintf(debugStream, "Incoming channel: %s\n", newChannel.ChannelType())
		if newChannel.ChannelType() != "session" {
			newChannel.Reject(ssh.UnknownChannelType, "unknown channel type")
			fmt.Fprintf(debugStream, "Unknown channel type: %s\n", newChannel.ChannelType())
			continue
		}
		channel, requests, err := newChannel.Accept()
		if err != nil {
			lg.Fatal().Err(err).Msg("could not accept channel.")
		}
		fmt.Fprintf(debugStream, "Channel accepted\n")

		// Sessions have out-of-band requests such as "shell",
		// "pty-req" and "env".  Here we handle only the
		// "subsystem" request.
		go func(in <-chan *ssh.Request) {
			for req := range in {
				fmt.Fprintf(debugStream, "Request: %v\n", req.Type)
				ok := false
				switch req.Type {
				case "subsystem":
					fmt.Fprintf(debugStream, "Subsystem: %s\n", req.Payload[4:])
					if string(req.Payload[4:]) == "sftp" {
						ok = true
					}
				}
				fmt.Fprintf(debugStream, " - accepted: %v\n", ok)
				req.Reply(ok, nil)
			}
		}(requests)

		root := sftp.InMemHandler()
		server := sftp.NewRequestServer(channel, root)
		if err := server.Serve(); err != nil {
			if err != io.EOF {
				lg.Fatal().Err(err).Msg("sftp server completed with error:")
			}
		}
		server.Close()
		lg.Info().Msg("sftp client exited session.")
	}
}
