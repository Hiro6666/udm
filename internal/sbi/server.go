package sbi

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/free5gc/openapi/models"
	"github.com/free5gc/udm/pkg/app"
	"github.com/free5gc/udm/pkg/factory"
	"github.com/free5gc/util/httpwrapper"
	"github.com/sirupsen/logrus"

	udm_context "github.com/free5gc/udm/internal/context"
	"github.com/free5gc/udm/internal/logger"
	"github.com/free5gc/udm/internal/sbi/consumer"
	"github.com/free5gc/udm/internal/sbi/processor"
	"github.com/free5gc/udm/internal/util"
	logger_util "github.com/free5gc/util/logger"
	"github.com/gin-gonic/gin"
)

type udm interface {
	Config() *factory.Config
	Context() *udm_context.UdmNFContext
	CancelContext() context.Context
	//Consumer() *consumer.Consumer
	// Processor() *processor.Processor
}

type ServerUdm interface {
	app.App

	Consumer() *consumer.Consumer
	Processor() *processor.Processor
}

type Server struct {
	ServerUdm

	httpServer *http.Server
	router     *gin.Engine
}

func NewServer(udm ServerUdm, tlsKeyLogPath string) (*Server, error) {
	s := &Server{
		ServerUdm: udm,
		router:    logger_util.NewGinWithLogrus(logger.GinLog),
	}

	cfg := s.Config()
	bindAddr := cfg.GetSbiBindingAddr()
	logger.SBILog.Infof("Binding addr: [%s]", bindAddr)
	var err error
	if s.httpServer, err = httpwrapper.NewHttp2Server(bindAddr, tlsKeyLogPath, s.router); err != nil {
		logger.InitLog.Errorf("Initialize HTTP server failed: %v", err)
		return nil, err
	}
	s.httpServer.ErrorLog = log.New(logger.SBILog.WriterLevel(logrus.ErrorLevel), "HTTP2: ", 0)

	/*server, err := bindRouter(s.Config(), s.router, tlsKeyLogPath)
	s.httpServer = server

	if err != nil {
		logger.SBILog.Errorf("bind Router Error: %+v", err)
		panic("Server initialization failed")
	}*/

	return s, err

}

func (s *Server) Run(traceCtx context.Context, wg *sync.WaitGroup) error {
	logger.SBILog.Info("Starting server...")

	var err error
	_, s.Context().NfId, err = s.Consumer().RegisterNFInstance(context.Background())
	if err != nil {
		logger.InitLog.Errorf("UDM register to NRF Error[%s]", err.Error())
	}

	wg.Add(1)
	go s.startServer(wg)
	/*go func() {
		defer wg.Done()

		err := s.serve()
		if err != http.ErrServerClosed {
			logger.SBILog.Panicf("HTTP server setup failed: %+v", err)
		}
	}()*/

	return nil
}

func (s *Server) startServer(wg *sync.WaitGroup) {
	defer func() {
		if p := recover(); p != nil {
			// Print stack for panic to log. Fatalf() will let program exit.
			logger.SBILog.Fatalf("panic: %v\n%s", p, string(debug.Stack()))
			s.Terminate()
		}
		wg.Done()
	}()

	logger.SBILog.Infof("Start SBI server (listen on %s)", s.httpServer.Addr)

	s.router = newRouter(s)

	var err error
	cfg := s.Config()
	scheme := cfg.GetSbiScheme()
	if scheme == "http" {
		err = s.httpServer.ListenAndServe()
	} else if scheme == "https" {
		err = s.httpServer.ListenAndServeTLS(
			cfg.GetCertPemPath(),
			cfg.GetCertKeyPath())
	} else {
		err = fmt.Errorf("No support this scheme[%s]", scheme)
	}

	if err != nil && err != http.ErrServerClosed {
		logger.SBILog.Errorf("SBI server error: %v", err)
	}
	logger.SBILog.Warnf("SBI server (listen on %s) stopped", s.httpServer.Addr)
}

func (s *Server) Shutdown() {
	s.shutdownHttpServer()
}

func (s *Server) Stop() {
	const defaultShutdownTimeout time.Duration = 2 * time.Second

	if s.httpServer != nil {
		logger.SBILog.Infof("Stop SBI server (listen on %s)", s.httpServer.Addr)
		toCtx, cancel := context.WithTimeout(context.Background(), defaultShutdownTimeout)
		defer cancel()
		if err := s.httpServer.Shutdown(toCtx); err != nil {
			logger.SBILog.Errorf("Could not close SBI server: %#v", err)
		}
	}
}

func (s *Server) shutdownHttpServer() {
	const shutdownTimeout time.Duration = 2 * time.Second

	if s.httpServer == nil {
		return
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	err := s.httpServer.Shutdown(shutdownCtx)
	if err != nil {
		logger.SBILog.Errorf("HTTP server shutdown failed: %+v", err)
	}
}

func bindRouter(cfg *factory.Config, router *gin.Engine, tlsKeyLogPath string) (*http.Server, error) {
	sbiConfig := cfg.Configuration.Sbi
	bindAddr := fmt.Sprintf("%s:%d", sbiConfig.BindingIPv4, sbiConfig.Port)

	return httpwrapper.NewHttp2Server(bindAddr, tlsKeyLogPath, router)
}

func newRouter(s *Server) *gin.Engine {
	router := logger_util.NewGinWithLogrus(logger.GinLog)

	udmEERoutes := s.getEventExposureRoutes()
	udmEEGroup := s.router.Group(factory.UdmEeResUriPrefix)
	routerAuthorizationCheck := util.NewRouterAuthorizationCheck(models.ServiceName_NUDM_EE)
	udmEEGroup.Use(func(c *gin.Context) {
		routerAuthorizationCheck.Check(c, udm_context.GetSelf())
	})
	AddService(udmEEGroup, udmEERoutes)

	udmCallBackRoutes := s.getHttpCallBackRoutes()
	udmCallNackGroup := s.router.Group("")
	AddService(udmCallNackGroup, udmCallBackRoutes)

	udmUEAURoutes := s.getUEAuthenticationRoutes()
	udmUEAUGroup := s.router.Group(factory.UdmUeauResUriPrefix)
	routerAuthorizationCheck = util.NewRouterAuthorizationCheck(models.ServiceName_NUDM_UEAU)
	udmUEAUGroup.Use(func(c *gin.Context) {
		routerAuthorizationCheck.Check(c, udm_context.GetSelf())
	})
	AddService(udmUEAUGroup, udmUEAURoutes)

	udmUECMRoutes := s.getUEContextManagementRoutes()
	udmUECMGroup := s.router.Group(factory.UdmUecmResUriPrefix)
	routerAuthorizationCheck = util.NewRouterAuthorizationCheck(models.ServiceName_NUDM_UECM)
	udmUECMGroup.Use(func(c *gin.Context) {
		routerAuthorizationCheck.Check(c, udm_context.GetSelf())
	})
	AddService(udmUECMGroup, udmUECMRoutes)

	udmSDMRoutes := s.getSubscriberDataManagementRoutes()
	udmSDMGroup := s.router.Group(factory.UdmSdmResUriPrefix)
	routerAuthorizationCheck = util.NewRouterAuthorizationCheck(models.ServiceName_NUDM_SDM)
	udmSDMGroup.Use(func(c *gin.Context) {
		routerAuthorizationCheck.Check(c, udm_context.GetSelf())
	})
	AddService(udmSDMGroup, udmSDMRoutes)

	udmPPRoutes := s.getParameterProvisionRoutes()
	udmPPGroup := s.router.Group(factory.UdmPpResUriPrefix)
	routerAuthorizationCheck = util.NewRouterAuthorizationCheck(models.ServiceName_NUDM_PP)
	udmPPGroup.Use(func(c *gin.Context) {
		routerAuthorizationCheck.Check(c, udm_context.GetSelf())
	})
	AddService(udmPPGroup, udmPPRoutes)

	return router
}

func (s *Server) unsecureServe() error {
	return s.httpServer.ListenAndServe()
}

func (s *Server) secureServe() error {
	sbiConfig := s.Config().Configuration.Sbi

	pemPath := sbiConfig.Tls.Pem
	if pemPath == "" {
		pemPath = factory.UdmConfig.GetCertKeyPath()
	}

	keyPath := sbiConfig.Tls.Key
	if keyPath == "" {
		keyPath = factory.UdmConfig.GetCertKeyPath()
	}

	return s.httpServer.ListenAndServeTLS(pemPath, keyPath)
}
