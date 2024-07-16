package cmd

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"golang.org/x/crypto/acme/autocert"

	"github.com/netbirdio/netbird/signal/metrics"

	"github.com/netbirdio/netbird/encryption"
	"github.com/netbirdio/netbird/signal/proto"
	"github.com/netbirdio/netbird/signal/server"
	"github.com/netbirdio/netbird/util"
	"github.com/netbirdio/netbird/version"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
)

const (
	metricsPort = 9090
)

var (
	signalPort              int
	signalLetsencryptDomain string
	signalSSLDir            string
	defaultSignalSSLDir     string
	signalCertFile          string
	signalCertKey           string
	enableCompatServer		bool

	signalKaep = grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
		MinTime:             5 * time.Second,
		PermitWithoutStream: true,
	})

	signalKasp = grpc.KeepaliveParams(keepalive.ServerParameters{
		MaxConnectionIdle:     15 * time.Second,
		MaxConnectionAgeGrace: 5 * time.Second,
		Time:                  5 * time.Second,
		Timeout:               2 * time.Second,
	})

	runCmd = &cobra.Command{
		Use:   "run",
		Short: "start NetBird Signal Server daemon",
		PreRun: func(cmd *cobra.Command, args []string) {
			flag.Parse()

			// detect whether user specified a port
			userPort := cmd.Flag("port").Changed

			tlsEnabled := false
			if signalLetsencryptDomain != "" || (signalCertFile != "" && signalCertKey != "") {
				tlsEnabled = true
			}

			if !userPort {
				// different defaults for signalPort
				if tlsEnabled {
					signalPort = 443
				} else {
					signalPort = 80
				}
			}
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			flag.Parse()

			err := util.InitLog(logLevel, logFile)
			if err != nil {
				log.Fatalf("failed initializing log %v", err)
			}

			if signalSSLDir == "" {
				oldPath := "/var/lib/wiretrustee"
				if migrateToNetbird(oldPath, defaultSignalSSLDir) {
					if err := cpDir(oldPath, defaultSignalSSLDir); err != nil {
						log.Fatal(err)
					}
				}
			}

			var opts []grpc.ServerOption
			var certManager *autocert.Manager
			var tlsConfig *tls.Config
			if signalLetsencryptDomain != "" {
				certManager, err = encryption.CreateCertManager(signalSSLDir, signalLetsencryptDomain)
				if err != nil {
					return err
				}
				transportCredentials := credentials.NewTLS(certManager.TLSConfig())
				opts = append(opts, grpc.Creds(transportCredentials))
				log.Infof("setting up TLS with LetsEncrypt.")
			} else if signalCertFile != "" && signalCertKey != "" {
				tlsConfig, err = loadTLSConfig(signalCertFile, signalCertKey)
				if err != nil {
					log.Errorf("cannot load TLS credentials: %v", err)
					return err
				}
				transportCredentials := credentials.NewTLS(tlsConfig)
				opts = append(opts, grpc.Creds(transportCredentials))
				log.Infof("setting up TLS with custom certificates.")
			}

			metricsServer := metrics.NewServer(metricsPort, "")
			if err != nil {
				return fmt.Errorf("setup metrics: %v", err)
			}

			opts = append(opts, signalKaep, signalKasp, grpc.StatsHandler(otelgrpc.NewServerHandler()))
			grpcServer := grpc.NewServer(opts...)

			go func() {
				log.Infof("running metrics server: %s%s", metricsServer.Addr, metricsServer.Endpoint)
				if err := metricsServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
					log.Fatalf("Failed to start metrics server: %v", err)
				}
			}()

			srv, err := server.NewServer(metricsServer.Meter)
			if err != nil {
				return fmt.Errorf("creating signal server: %v", err)
			}
			proto.RegisterSignalExchangeServer(grpcServer, srv)

			grpcRootHandler := grpcHandlerFunc(grpcServer)
			var compatListener net.Listener
			var grpcListener net.Listener
			var httpListener net.Listener

			if certManager != nil {
				// a call to certManager.Listener() always creates a new listener so we do it once
				httpListener := certManager.Listener()
				if signalPort == 443 {
					// running gRPC and HTTP cert manager on the same port
					serveHTTP(httpListener, certManager.HTTPHandler(grpcRootHandler))
					log.Infof("running HTTP server (LetsEncrypt challenge handler) and gRPC server on the same port: %s", httpListener.Addr().String())
				} else {
					// Start the HTTP cert manager server separately
					serveHTTP(httpListener, certManager.HTTPHandler(nil))
					log.Infof("running HTTP server (LetsEncrypt challenge handler): %s", httpListener.Addr().String())
				}
			}

			// If certManager is configured and signalPort == 443, then the gRPC server has already been started
			if certManager == nil || signalPort != 443 {
				grpcListener, err = serveGRPC(grpcServer, signalPort)
				if err != nil {
					return err
				}
				log.Infof("running gRPC server: %s", grpcListener.Addr().String())
			}

			if enableCompatServer && signalPort != 10000 {
				// The Signal gRPC server was running on port 10000 previously. Old agents that are already connected to Signal
				// are using port 10000. For compatibility purposes we keep running a 2nd gRPC server on port 10000.
				compatListener, err = serveGRPC(grpcServer, 10000)
				if err != nil {
					return err
				}
				log.Infof("running gRPC backward compatibility server: %s", compatListener.Addr().String())
			}

			log.Infof("signal server version %s", version.NetbirdVersion())
			log.Infof("started Signal Service")

			SetupCloseHandler()

			<-stopCh
			if grpcListener != nil {
				_ = grpcListener.Close()
				log.Infof("stopped gRPC server")
			}
			if httpListener != nil {
				_ = httpListener.Close()
				log.Infof("stopped HTTP server")
			}
			if compatListener != nil {
				_ = compatListener.Close()
				log.Infof("stopped gRPC backward compatibility server")
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()
			if err := metricsServer.Shutdown(ctx); err != nil {
				log.Errorf("Failed to stop metrics server: %v", err)
			}
			log.Infof("stopped metrics server")

			log.Infof("stopped Signal Service")

			return nil
		},
	}
)

func grpcHandlerFunc(grpcServer *grpc.Server) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		grpcHeader := strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") ||
			strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc+proto")
		if r.ProtoMajor == 2 && grpcHeader {
			grpcServer.ServeHTTP(w, r)
		}
	})
}

func notifyStop(msg string) {
	select {
	case stopCh <- 1:
		log.Error(msg)
	default:
		// stop has been already called, nothing to report
	}
}

func serveHTTP(httpListener net.Listener, handler http.Handler) {
	go func() {
		err := http.Serve(httpListener, handler)
		if err != nil {
			notifyStop(fmt.Sprintf("failed running HTTP server %v", err))
		}
	}()
}

func serveGRPC(grpcServer *grpc.Server, port int) (net.Listener, error) {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, err
	}
	go func() {
		err := grpcServer.Serve(listener)
		if err != nil {
			notifyStop(fmt.Sprintf("failed running gRPC server on port %d: %v", port, err))
		}
	}()
	return listener, nil
}

func loadTLSConfig(certFile string, certKey string) (*tls.Config, error) {
	// Load server's certificate and private key
	serverCert, err := tls.LoadX509KeyPair(certFile, certKey)
	if err != nil {
		return nil, err
	}

	// NewDefaultAppMetrics the credentials and return it
	config := &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.NoClientCert,
		NextProtos: []string{
			"h2", "http/1.1", // enable HTTP/2
		},
	}

	return config, nil
}

func cpFile(src, dst string) error {
	var err error
	var srcfd *os.File
	var dstfd *os.File
	var srcinfo os.FileInfo

	if srcfd, err = os.Open(src); err != nil {
		return err
	}
	defer srcfd.Close()

	if dstfd, err = os.Create(dst); err != nil {
		return err
	}
	defer dstfd.Close()

	if _, err = io.Copy(dstfd, srcfd); err != nil {
		return err
	}
	if srcinfo, err = os.Stat(src); err != nil {
		return err
	}
	return os.Chmod(dst, srcinfo.Mode())
}

func copySymLink(source, dest string) error {
	link, err := os.Readlink(source)
	if err != nil {
		return err
	}
	return os.Symlink(link, dest)
}

func cpDir(src string, dst string) error {
	var err error
	var fds []os.DirEntry
	var srcinfo os.FileInfo

	if srcinfo, err = os.Stat(src); err != nil {
		return err
	}

	if err = os.MkdirAll(dst, srcinfo.Mode()); err != nil {
		return err
	}

	if fds, err = os.ReadDir(src); err != nil {
		return err
	}
	for _, fd := range fds {
		srcfp := path.Join(src, fd.Name())
		dstfp := path.Join(dst, fd.Name())

		fileInfo, err := os.Stat(srcfp)
		if err != nil {
			log.Fatalf("Couldn't get fileInfo; %v", err)
		}

		switch fileInfo.Mode() & os.ModeType {
		case os.ModeSymlink:
			if err = copySymLink(srcfp, dstfp); err != nil {
				log.Fatalf("Failed to copy from %s to %s; %v", srcfp, dstfp, err)
			}
		case os.ModeDir:
			if err = cpDir(srcfp, dstfp); err != nil {
				log.Fatalf("Failed to copy from %s to %s; %v", srcfp, dstfp, err)
			}
		default:
			if err = cpFile(srcfp, dstfp); err != nil {
				log.Fatalf("Failed to copy from %s to %s; %v", srcfp, dstfp, err)
			}
		}
	}
	return nil
}

func migrateToNetbird(oldPath, newPath string) bool {
	_, errOld := os.Stat(oldPath)
	_, errNew := os.Stat(newPath)

	if errors.Is(errOld, fs.ErrNotExist) || errNew == nil {
		return false
	}

	return true
}

func init() {
	runCmd.PersistentFlags().IntVar(&signalPort, "port", 80, "Server port to listen on (defaults to 443 if TLS is enabled, 80 otherwise")
	runCmd.Flags().StringVar(&signalSSLDir, "ssl-dir", defaultSignalSSLDir, "server ssl directory location. *Required only for Let's Encrypt certificates.")
	runCmd.Flags().StringVar(&signalLetsencryptDomain, "letsencrypt-domain", "", "a domain to issue Let's Encrypt certificate for. Enables TLS using Let's Encrypt. Will fetch and renew certificate, and run the server with TLS")
	runCmd.Flags().StringVar(&signalCertFile, "cert-file", "", "Location of your SSL certificate. Can be used when you have an existing certificate and don't want a new certificate be generated automatically. If letsencrypt-domain is specified this property has no effect")
	runCmd.Flags().StringVar(&signalCertKey, "cert-key", "", "Location of your SSL certificate private key. Can be used when you have an existing certificate and don't want a new certificate be generated automatically. If letsencrypt-domain is specified this property has no effect")
	runCmd.Flags().BoolVar(&enableCompatServer, "enable-compat-server", false, "Enables a second server which listens on port 10000 for compatability with older, pre-existing clients. If port is set to 10000, this setting has no effect.")
}
