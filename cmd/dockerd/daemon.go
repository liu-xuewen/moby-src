package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	containerddefaults "github.com/containerd/containerd/defaults"
	"github.com/docker/docker/api"
	apiserver "github.com/docker/docker/api/server"
	buildbackend "github.com/docker/docker/api/server/backend/build"
	"github.com/docker/docker/api/server/middleware"
	"github.com/docker/docker/api/server/router"
	"github.com/docker/docker/api/server/router/build"
	checkpointrouter "github.com/docker/docker/api/server/router/checkpoint"
	"github.com/docker/docker/api/server/router/container"
	distributionrouter "github.com/docker/docker/api/server/router/distribution"
	grpcrouter "github.com/docker/docker/api/server/router/grpc"
	"github.com/docker/docker/api/server/router/image"
	"github.com/docker/docker/api/server/router/network"
	pluginrouter "github.com/docker/docker/api/server/router/plugin"
	sessionrouter "github.com/docker/docker/api/server/router/session"
	swarmrouter "github.com/docker/docker/api/server/router/swarm"
	systemrouter "github.com/docker/docker/api/server/router/system"
	"github.com/docker/docker/api/server/router/volume"
	buildkit "github.com/docker/docker/builder/builder-next"
	"github.com/docker/docker/builder/dockerfile"
	"github.com/docker/docker/cli/debug"
	"github.com/docker/docker/daemon"
	"github.com/docker/docker/daemon/cluster"
	"github.com/docker/docker/daemon/config"
	"github.com/docker/docker/daemon/listeners"
	"github.com/docker/docker/dockerversion"
	"github.com/docker/docker/libcontainerd/supervisor"
	dopts "github.com/docker/docker/opts"
	"github.com/docker/docker/pkg/authorization"
	"github.com/docker/docker/pkg/homedir"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/docker/docker/pkg/pidfile"
	"github.com/docker/docker/pkg/plugingetter"
	"github.com/docker/docker/pkg/signal"
	"github.com/docker/docker/pkg/sysinfo"
	"github.com/docker/docker/pkg/system"
	"github.com/docker/docker/plugin"
	"github.com/docker/docker/rootless"
	"github.com/docker/docker/runconfig"
	"github.com/docker/go-connections/tlsconfig"
	swarmapi "github.com/docker/swarmkit/api"
	"github.com/moby/buildkit/session"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
)

// DaemonCli represents the daemon CLI.
type DaemonCli struct {
	*config.Config
	configFile *string
	flags      *pflag.FlagSet

	api             *apiserver.Server
	d               *daemon.Daemon
	authzMiddleware *authorization.Middleware // authzMiddleware enables to dynamically reload the authorization plugins
}

// NewDaemonCli returns a daemon CLI
func NewDaemonCli() *DaemonCli {
	return &DaemonCli{}
}

func (cli *DaemonCli) start(opts *daemonOptions) (err error) {
	stopc := make(chan bool)
	defer close(stopc)

	opts.SetDefaultOptions(opts.flags)

	if cli.Config, err = loadDaemonCliConfig(opts); err != nil {
		return err
	}
	warnOnDeprecatedConfigOptions(cli.Config)

	if err := configureDaemonLogs(cli.Config); err != nil {
		return err
	}

	// -D的第一条日志
	logrus.Info("Starting up")

	cli.configFile = &opts.configFile
	cli.flags = opts.flags

	if cli.Config.Debug {
		debug.Enable()
	}

	if cli.Config.Experimental {
		logrus.Warn("Running experimental build")
	}

	if cli.Config.IsRootless() {
		logrus.Warn("Running in rootless mode. This mode has feature limitations.")
	}
	if rootless.RunningWithRootlessKit() {
		logrus.Info("Running with RootlessKit integration")
		if !cli.Config.IsRootless() {
			return fmt.Errorf("rootless mode needs to be enabled for running with RootlessKit")
		}
	}

	// return human-friendly error before creating files
	if runtime.GOOS == "linux" && os.Geteuid() != 0 {
		return fmt.Errorf("dockerd needs to be started with root. To see how to run dockerd in rootless mode with unprivileged user, see the documentation")
	}

	system.InitLCOW(cli.Config.Experimental)

	if err := setDefaultUmask(); err != nil {
		return err
	}

	// Create the daemon root before we create ANY other files (PID, or migrate keys)
	// to ensure the appropriate ACL is set (particularly relevant on Windows)
	/*
	在创建任何其他文件(PID或迁移密钥)之前创建守护程序根目录，以确保设置了适当的ACL(尤其与Windows相关)
	*/
	if err := daemon.CreateDaemonRoot(cli.Config); err != nil {
		return err
	}

	if err := system.MkdirAll(cli.Config.ExecRoot, 0700); err != nil {
		return err
	}

	potentiallyUnderRuntimeDir := []string{cli.Config.ExecRoot}

	if cli.Pidfile != "" {
		pf, err := pidfile.New(cli.Pidfile)
		if err != nil {
			return errors.Wrap(err, "failed to start daemon")
		}
		potentiallyUnderRuntimeDir = append(potentiallyUnderRuntimeDir, cli.Pidfile)
		defer func() {
			if err := pf.Remove(); err != nil {
				logrus.Error(err)
			}
		}()
	}

	if cli.Config.IsRootless() {
		// Set sticky bit if XDG_RUNTIME_DIR is set && the file is actually under XDG_RUNTIME_DIR
		/*
		如果设置了XDG_RUNTIME_DIR，则设置粘滞位&文件实际上位于XDG_RUNTIME_DIR下
		*/
		if _, err := homedir.StickRuntimeDirContents(potentiallyUnderRuntimeDir); err != nil {
			// StickRuntimeDirContents returns nil error if XDG_RUNTIME_DIR is just unset
			logrus.WithError(err).Warn("cannot set sticky bit on files under XDG_RUNTIME_DIR")
		}
	}

	serverConfig, err := newAPIServerConfig(cli)
	if err != nil {
		return errors.Wrap(err, "failed to create API server")
	}
	// New根据指定的配置返回服务器的新实例。
	// 它分配ServeAPI(端口、Unix套接字)所需的资源。
	cli.api = apiserver.New(serverConfig)

	// 这里要加载 docker.sock服务监听
	// 如果dockerd启动，直接listen unix文件会自动创建unix监听文件，该文件存在则报错
	// 并将server router设置到accept里面
	hosts, err := loadListeners(cli, serverConfig)
	if err != nil {
		// 直接运行 /usr/bin/dockerd -H fd:// 会报错
		panic("failed to load listeners")
		return errors.Wrap(err, "failed to load listeners")
	}

	ctx, cancel := context.WithCancel(context.Background())
	waitForContainerDShutdown, err := cli.initContainerD(ctx)
	if waitForContainerDShutdown != nil {
		defer waitForContainerDShutdown(10 * time.Second)
	}
	if err != nil {
		cancel()
		return err
	}
	defer cancel()

	signal.Trap(func() {
		cli.stop()
		<-stopc // wait for daemonCli.start() to return
	}, logrus.StandardLogger())

	// Notify that the API is active, but before daemon is set up.
	preNotifySystem()

	pluginStore := plugin.NewStore()

	if err := cli.initMiddlewares(cli.api, serverConfig, pluginStore); err != nil {
		logrus.Fatalf("Error creating middlewares: %v", err)
	}

	d, err := daemon.NewDaemon(ctx, cli.Config, pluginStore)
	if err != nil {
		return errors.Wrap(err, "failed to start daemon")
	}

	d.StoreHosts(hosts)

	// validate after NewDaemon has restored enabled plugins. Don't change order.
	if err := validateAuthzPlugins(cli.Config.AuthorizationPlugins, pluginStore); err != nil {
		return errors.Wrap(err, "failed to validate authorization plugin")
	}

	cli.d = d

	if err := startMetricsServer(cli.Config.MetricsAddress); err != nil {
		return errors.Wrap(err, "failed to start metrics server")
	}

	c, err := createAndStartCluster(cli, d)
	if err != nil {
		logrus.Fatalf("Error starting cluster component: %v", err)
	}

	// Restart all autostart containers which has a swarm endpoint
	// and is not yet running now that we have successfully
	// initialized the cluster.
	/*
	重新启动所有具有群集终结点且尚未运行的自动启动容器，因为我们已经成功初始化了群集。
	*/
	d.RestartSwarmContainers()

	logrus.Info("Daemon has completed initialization")

	routerOptions, err := newRouterOptions(cli.Config, d)
	if err != nil {
		return err
	}
	routerOptions.api = cli.api
	routerOptions.cluster = c

	initRouter(routerOptions)

	go d.ProcessClusterNotifications(ctx, c.GetWatchStream())

	cli.setupConfigReloadTrap()

	// The serve API routine never exits unless an error occurs
	// We need to start it as a goroutine and wait on it so
	// daemon doesn't exit
	/*
	除非发生错误，否则Serve API例程永远不会退出。
	我们需要将其作为goroutine启动并等待，这样守护进程才不会退出
	*/
	serveAPIWait := make(chan error)
	go cli.api.Wait(serveAPIWait)

	// after the daemon is done setting up we can notify systemd api
	notifySystem()

	// Daemon is fully initialized and handling API traffic
	// Wait for serve API to complete
	/*
	后台进程已完全初始化，正在处理API流量，等待服务API完成
	*/
	errAPI := <-serveAPIWait
	c.Cleanup()

	shutdownDaemon(d)

	// Stop notification processing and any background processes
	cancel()

	if errAPI != nil {
		return errors.Wrap(errAPI, "shutting down due to ServeAPI error")
	}

	logrus.Info("Daemon shutdown complete")
	return nil
}

type routerOptions struct {
	sessionManager *session.Manager
	buildBackend   *buildbackend.Backend
	features       *map[string]bool
	buildkit       *buildkit.Builder
	daemon         *daemon.Daemon
	api            *apiserver.Server
	cluster        *cluster.Cluster
}

func newRouterOptions(config *config.Config, d *daemon.Daemon) (routerOptions, error) {
	opts := routerOptions{}
	sm, err := session.NewManager()
	if err != nil {
		return opts, errors.Wrap(err, "failed to create sessionmanager")
	}

	manager, err := dockerfile.NewBuildManager(d.BuilderBackend(), d.IdentityMapping())
	if err != nil {
		return opts, err
	}
	cgroupParent := newCgroupParent(config)
	bk, err := buildkit.New(buildkit.Opt{
		SessionManager:      sm,
		Root:                filepath.Join(config.Root, "buildkit"),
		Dist:                d.DistributionServices(),
		NetworkController:   d.NetworkController(),
		DefaultCgroupParent: cgroupParent,
		RegistryHosts:       d.RegistryHosts(),
		BuilderConfig:       config.Builder,
		Rootless:            d.Rootless(),
		IdentityMapping:     d.IdentityMapping(),
		DNSConfig:           config.DNSConfig,
	})
	if err != nil {
		return opts, err
	}

	bb, err := buildbackend.NewBackend(d.ImageService(), manager, bk)
	if err != nil {
		return opts, errors.Wrap(err, "failed to create buildmanager")
	}
	return routerOptions{
		sessionManager: sm,
		buildBackend:   bb,
		buildkit:       bk,
		features:       d.Features(),
		daemon:         d,
	}, nil
}

func (cli *DaemonCli) reloadConfig() {
	reload := func(c *config.Config) {

		// Revalidate and reload the authorization plugins
		if err := validateAuthzPlugins(c.AuthorizationPlugins, cli.d.PluginStore); err != nil {
			logrus.Fatalf("Error validating authorization plugin: %v", err)
			return
		}
		cli.authzMiddleware.SetPlugins(c.AuthorizationPlugins)

		if err := cli.d.Reload(c); err != nil {
			logrus.Errorf("Error reconfiguring the daemon: %v", err)
			return
		}

		if c.IsValueSet("debug") {
			debugEnabled := debug.IsEnabled()
			switch {
			case debugEnabled && !c.Debug: // disable debug
				debug.Disable()
			case c.Debug && !debugEnabled: // enable debug
				debug.Enable()
			}
		}
	}

	if err := config.Reload(*cli.configFile, cli.flags, reload); err != nil {
		logrus.Error(err)
	}
}

func (cli *DaemonCli) stop() {
	cli.api.Close()
}

// shutdownDaemon just wraps daemon.Shutdown() to handle a timeout in case
// d.Shutdown() is waiting too long to kill container or worst it's
// blocked there
func shutdownDaemon(d *daemon.Daemon) {
	shutdownTimeout := d.ShutdownTimeout()
	ch := make(chan struct{})
	go func() {
		d.Shutdown()
		close(ch)
	}()
	if shutdownTimeout < 0 {
		<-ch
		logrus.Debug("Clean shutdown succeeded")
		return
	}

	timeout := time.NewTimer(time.Duration(shutdownTimeout) * time.Second)
	defer timeout.Stop()

	select {
	case <-ch:
		logrus.Debug("Clean shutdown succeeded")
	case <-timeout.C:
		logrus.Error("Force shutdown daemon")
	}
}

func loadDaemonCliConfig(opts *daemonOptions) (*config.Config, error) {
	conf := opts.daemonConfig
	flags := opts.flags
	conf.Debug = opts.Debug
	conf.Hosts = opts.Hosts
	conf.LogLevel = opts.LogLevel
	conf.TLS = opts.TLS
	conf.TLSVerify = opts.TLSVerify
	conf.CommonTLSOptions = config.CommonTLSOptions{}

	if opts.TLSOptions != nil {
		conf.CommonTLSOptions.CAFile = opts.TLSOptions.CAFile
		conf.CommonTLSOptions.CertFile = opts.TLSOptions.CertFile
		conf.CommonTLSOptions.KeyFile = opts.TLSOptions.KeyFile
	}

	if conf.TrustKeyPath == "" {
		daemonConfDir, err := getDaemonConfDir(conf.Root)
		if err != nil {
			return nil, err
		}
		conf.TrustKeyPath = filepath.Join(daemonConfDir, defaultTrustKeyFile)
	}

	if flags.Changed("graph") && flags.Changed("data-root") {
		return nil, errors.New(`cannot specify both "--graph" and "--data-root" option`)
	}

	if opts.configFile != "" {
		c, err := config.MergeDaemonConfigurations(conf, flags, opts.configFile)
		if err != nil {
			if flags.Changed("config-file") || !os.IsNotExist(err) {
				return nil, errors.Wrapf(err, "unable to configure the Docker daemon with file %s", opts.configFile)
			}
		}
		// the merged configuration can be nil if the config file didn't exist.
		// leave the current configuration as it is if when that happens.
		if c != nil {
			conf = c
		}
	}

	if err := config.Validate(conf); err != nil {
		return nil, err
	}

	if flags.Changed("graph") {
		logrus.Warnf(`The "-g / --graph" flag is deprecated. Please use "--data-root" instead`)
	}

	// Check if duplicate label-keys with different values are found
	newLabels, err := config.GetConflictFreeLabels(conf.Labels)
	if err != nil {
		return nil, err
	}
	conf.Labels = newLabels

	// Regardless of whether the user sets it to true or false, if they
	// specify TLSVerify at all then we need to turn on TLS
	if conf.IsValueSet(FlagTLSVerify) {
		conf.TLS = true
	}

	return conf, nil
}

func warnOnDeprecatedConfigOptions(config *config.Config) {
	if config.ClusterAdvertise != "" {
		logrus.Warn(`The "cluster-advertise" option is deprecated. To be removed soon.`)
	}
	if config.ClusterStore != "" {
		logrus.Warn(`The "cluster-store" option is deprecated. To be removed soon.`)
	}
	if len(config.ClusterOpts) > 0 {
		logrus.Warn(`The "cluster-store-opt" option is deprecated. To be removed soon.`)
	}
}

func initRouter(opts routerOptions) {
	decoder := runconfig.ContainerDecoder{
		GetSysInfo: func() *sysinfo.SysInfo {
			return opts.daemon.RawSysInfo(true)
		},
	}

	routers := []router.Router{
		// we need to add the checkpoint router before the container router or the DELETE gets masked
		// 我们需要在容器路由器或删除被屏蔽之前添加检查点路由器
		checkpointrouter.NewRouter(opts.daemon, decoder),
		container.NewRouter(opts.daemon, decoder),
		image.NewRouter(opts.daemon.ImageService()),
		systemrouter.NewRouter(opts.daemon, opts.cluster, opts.buildkit, opts.features),
		volume.NewRouter(opts.daemon.VolumesService()),
		build.NewRouter(opts.buildBackend, opts.daemon, opts.features),
		sessionrouter.NewRouter(opts.sessionManager),
		swarmrouter.NewRouter(opts.cluster),
		pluginrouter.NewRouter(opts.daemon.PluginManager()),
		distributionrouter.NewRouter(opts.daemon.ImageService()),
	}

	grpcBackends := []grpcrouter.Backend{}
	for _, b := range []interface{}{opts.daemon, opts.buildBackend} {
		if b, ok := b.(grpcrouter.Backend); ok {
			grpcBackends = append(grpcBackends, b)
		}
	}
	if len(grpcBackends) > 0 {
		routers = append(routers, grpcrouter.NewRouter(grpcBackends...))
	}

	if opts.daemon.NetworkControllerEnabled() {
		routers = append(routers, network.NewRouter(opts.daemon, opts.cluster))
	}

	if opts.daemon.HasExperimental() {
		for _, r := range routers {
			for _, route := range r.Routes() {
				if experimental, ok := route.(router.ExperimentalRoute); ok {
					experimental.Enable()
				}
			}
		}
	}

	opts.api.InitRouter(routers...)
}

// TODO: remove this from cli and return the authzMiddleware
func (cli *DaemonCli) initMiddlewares(s *apiserver.Server, cfg *apiserver.Config, pluginStore plugingetter.PluginGetter) error {
	v := cfg.Version

	exp := middleware.NewExperimentalMiddleware(cli.Config.Experimental)
	s.UseMiddleware(exp)

	vm := middleware.NewVersionMiddleware(v, api.DefaultVersion, api.MinVersion)
	s.UseMiddleware(vm)

	if cfg.CorsHeaders != "" {
		c := middleware.NewCORSMiddleware(cfg.CorsHeaders)
		s.UseMiddleware(c)
	}

	cli.authzMiddleware = authorization.NewMiddleware(cli.Config.AuthorizationPlugins, pluginStore)
	cli.Config.AuthzMiddleware = cli.authzMiddleware
	s.UseMiddleware(cli.authzMiddleware)
	return nil
}

func (cli *DaemonCli) getContainerdDaemonOpts() ([]supervisor.DaemonOpt, error) {
	opts, err := cli.getPlatformContainerdDaemonOpts()
	if err != nil {
		return nil, err
	}

	if cli.Config.Debug {
		opts = append(opts, supervisor.WithLogLevel("debug"))
	} else if cli.Config.LogLevel != "" {
		opts = append(opts, supervisor.WithLogLevel(cli.Config.LogLevel))
	}

	if !cli.Config.CriContainerd {
		opts = append(opts, supervisor.WithPlugin("cri", nil))
	}

	return opts, nil
}

func newAPIServerConfig(cli *DaemonCli) (*apiserver.Config, error) {
	serverConfig := &apiserver.Config{
		Logging:     true,
		SocketGroup: cli.Config.SocketGroup,
		Version:     dockerversion.Version,
		CorsHeaders: cli.Config.CorsHeaders,
	}

	if cli.Config.TLS {
		tlsOptions := tlsconfig.Options{
			CAFile:             cli.Config.CommonTLSOptions.CAFile,
			CertFile:           cli.Config.CommonTLSOptions.CertFile,
			KeyFile:            cli.Config.CommonTLSOptions.KeyFile,
			ExclusiveRootPools: true,
		}

		if cli.Config.TLSVerify {
			// server requires and verifies client's certificate
			tlsOptions.ClientAuth = tls.RequireAndVerifyClientCert
		}
		tlsConfig, err := tlsconfig.Server(tlsOptions)
		if err != nil {
			return nil, err
		}
		serverConfig.TLSConfig = tlsConfig
	}

	// 如果没有给他初始化为空字符串？
	if len(cli.Config.Hosts) == 0 {
		cli.Config.Hosts = make([]string, 1)
	}

	return serverConfig, nil
}

func loadListeners(cli *DaemonCli, serverConfig *apiserver.Config) ([]string, error) {
	var hosts []string
	seen := make(map[string]struct{}, len(cli.Config.Hosts))

	for i := 0; i < len(cli.Config.Hosts); i++ {
		var err error
		if cli.Config.Hosts[i], err = dopts.ParseHost(cli.Config.TLS, honorXDG, cli.Config.Hosts[i]); err != nil {
			return nil, errors.Wrapf(err, "error parsing -H %s", cli.Config.Hosts[i])
		}
		if _, ok := seen[cli.Config.Hosts[i]]; ok {
			continue
		}
		seen[cli.Config.Hosts[i]] = struct{}{}

		protoAddr := cli.Config.Hosts[i]
		// ExecStart=/usr/bin/dockerd -H fd://
		protoAddrParts := strings.SplitN(protoAddr, "://", 2)
		// fd是2
		if len(protoAddrParts) != 2 {
			return nil, fmt.Errorf("bad format %s, expected PROTO://ADDR", protoAddr)
		}

		proto := protoAddrParts[0]
		addr := protoAddrParts[1]

		// ExecStart=/usr/bin/dockerd -H fd://

		// It's a bad idea to bind to TCP without tlsverify.
		if proto == "tcp" && (serverConfig.TLSConfig == nil || serverConfig.TLSConfig.ClientAuth != tls.RequireAndVerifyClientCert) {
			logrus.Warn("[!] DON'T BIND ON ANY IP ADDRESS WITHOUT setting --tlsverify IF YOU DON'T KNOW WHAT YOU'RE DOING [!]")
		}

		// 如果dockerd启动，直接listen unix文件会自动创建unix监听文件，该文件存在则报错
		ls, err := listeners.Init(proto, addr, serverConfig.SocketGroup, serverConfig.TLSConfig)
		if err != nil {
			return nil, err
		}
		// If we're binding to a TCP port, make sure that a container doesn't try to use it.
		if proto == "tcp" {
			if err := allocateDaemonPort(addr); err != nil {
				return nil, err
			}
		}

		// 启动-D的第二条日志
		logrus.Debugf("Listener created for HTTP on %s (%s)", proto, addr)
		hosts = append(hosts, protoAddrParts[1])
		cli.api.Accept(addr, ls...)
	}

	return hosts, nil
}

func createAndStartCluster(cli *DaemonCli, d *daemon.Daemon) (*cluster.Cluster, error) {
	name, _ := os.Hostname()

	// Use a buffered channel to pass changes from store watch API to daemon
	// A buffer allows store watch API and daemon processing to not wait for each other
	watchStream := make(chan *swarmapi.WatchMessage, 32)

	c, err := cluster.New(cluster.Config{
		Root:                   cli.Config.Root,
		Name:                   name,
		Backend:                d,
		VolumeBackend:          d.VolumesService(),
		ImageBackend:           d.ImageService(),
		PluginBackend:          d.PluginManager(),
		NetworkSubnetsProvider: d,
		DefaultAdvertiseAddr:   cli.Config.SwarmDefaultAdvertiseAddr,
		RaftHeartbeatTick:      cli.Config.SwarmRaftHeartbeatTick,
		RaftElectionTick:       cli.Config.SwarmRaftElectionTick,
		RuntimeRoot:            cli.getSwarmRunRoot(),
		WatchStream:            watchStream,
	})
	if err != nil {
		return nil, err
	}
	d.SetCluster(c)
	err = c.Start()

	return c, err
}

// validates that the plugins requested with the --authorization-plugin flag are valid AuthzDriver
// plugins present on the host and available to the daemon
func validateAuthzPlugins(requestedPlugins []string, pg plugingetter.PluginGetter) error {
	for _, reqPlugin := range requestedPlugins {
		if _, err := pg.Get(reqPlugin, authorization.AuthZApiImplements, plugingetter.Lookup); err != nil {
			return err
		}
	}
	return nil
}

func systemContainerdRunning(honorXDG bool) (string, bool, error) {
	addr := containerddefaults.DefaultAddress
	if honorXDG {
		runtimeDir, err := homedir.GetRuntimeDir()
		if err != nil {
			return "", false, err
		}
		addr = filepath.Join(runtimeDir, "containerd", "containerd.sock")
	}
	_, err := os.Lstat(addr)
	return addr, err == nil, nil
}

// configureDaemonLogs sets the logrus logging level and formatting
func configureDaemonLogs(conf *config.Config) error {
	if conf.LogLevel != "" {
		lvl, err := logrus.ParseLevel(conf.LogLevel)
		if err != nil {
			return fmt.Errorf("unable to parse logging level: %s", conf.LogLevel)
		}
		logrus.SetLevel(lvl)
	} else {
		logrus.SetLevel(logrus.InfoLevel)
	}
	logrus.SetFormatter(&logrus.TextFormatter{
		TimestampFormat: jsonmessage.RFC3339NanoFixed,
		DisableColors:   conf.RawLogs,
		FullTimestamp:   true,
	})
	return nil
}
