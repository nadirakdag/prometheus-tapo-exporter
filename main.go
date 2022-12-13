package main

import (
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	log "github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/kr/pretty"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/promlog"
	"github.com/prometheus/common/promlog/flag"
	"github.com/prometheus/common/version"
	kingpin "gopkg.in/alecthomas/kingpin.v2"
	stdlog "log"

	tapo "github.com/paulcager/tapo-lib"
)

const (
	namespace = "tapo"
	subsystem = "device"
)

var (
	listenAddress          = kingpin.Flag("web.listen-address", "Address on which to expose metrics.").Default(":9782").String()
	metricsEndpoint        = kingpin.Flag("web.telemetry-path", "Path under which to expose metrics.").Default("/metrics").String()
	tapoUsername           = kingpin.Flag("tapo.username", "Tapo username.").Required().String()
	tapoPassword           = kingpin.Flag("tapo.password", "Tapo password.").Required().String()
	clientTimeout          = kingpin.Flag("http.timeout", "Timeout for HTTP call out into each device").Default("10s").Duration()
	disableExporterMetrics = kingpin.Flag(
		"web.disable-exporter-metrics",
		"Exclude metrics about the exporter itself (promhttp_*, process_*, go_*).",
	).Bool()
	tapoDevices = kingpin.Arg("devices", "Devices to query").Required().Strings()

	logger log.Logger

	_ = pretty.Print
)

func main() {
	kingpin.Version(version.Print("tapo_exporter"))
	promlogConfig := &promlog.Config{}
	flag.AddFlags(kingpin.CommandLine, promlogConfig)
	kingpin.HelpFlag.Short('h')
	kingpin.CommandLine.UsageWriter(os.Stdout)
	kingpin.Parse()

	logger = promlog.New(promlogConfig)
	level.Info(logger).Log("msg", "Starting tapo_exporter", "version", version.Info())
	level.Info(logger).Log("msg", "Build context", "build_context", version.BuildContext())

	var registry = prometheus.DefaultRegisterer
	var gatherer = prometheus.DefaultGatherer
	if *disableExporterMetrics {
		reg := prometheus.NewRegistry()
		registry = reg
		gatherer = reg
	}

	exporter, err := NewExporter()
	if err != nil {
		panic(err)
	}

	registry.MustRegister(exporter)
	registry.MustRegister(version.NewCollector("tapo_exporter"))

	http.Handle(*metricsEndpoint, promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{}))
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`
<html>
			<head><title>Tapo Exporter</title></head>
			<body>
			<h1>Tapo Exporter</h1>
			<p><a href="` + *metricsEndpoint + `">Metrics</a></p>
			</body>
</html>
`))
	})

	stdlog.Fatal(http.ListenAndServe(*listenAddress, nil))
}

type Device struct {
	sync.Mutex
	address       string
	session       *tapo.Session
	initialised   bool
	supportsPower bool

	lastWasValid bool

	up         prometheus.Gauge
	errors     prometheus.Counter
	on         prometheus.Gauge
	onTime     prometheus.Gauge
	overheated prometheus.Gauge

	// Power-management only
	currentPower   prometheus.Gauge
	todayRuntime   prometheus.Gauge
	todayWattHours prometheus.Gauge
}

func NewDevice(address string) (*Device, error) {
	dev := &Device{address: address}

	sess, err := tapo.NewSession(address, *tapoUsername, *tapoPassword)
	if err != nil {
		return nil, err
	}
	sess.Client = &http.Client{Timeout: *clientTimeout}

	dev.session = sess
	dev.up = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "up",
		Help:        "Is the device up",
		ConstLabels: map[string]string{"ip": address},
	})
	dev.errors = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace:   namespace,
		Subsystem:   subsystem,
		Name:        "errors",
		Help:        "Count of errors retrieving details",
		ConstLabels: map[string]string{"ip": address},
	})

	return dev, nil
}

func (d *Device) refresh() {
	d.Lock()
	defer d.Unlock()

	start := time.Now()

	info, err := d.session.GetDeviceInfo()
	if err != nil {
		level.Warn(logger).Log("device", d.address, "err", err, "time", time.Since(start).Seconds())
	} else {
		level.Debug(logger).Log("device", d.address, "on", info.DeviceOn, "time", time.Since(start).Seconds())
	}

	d.lastWasValid = err == nil

	if err != nil {
		d.up.Set(0)
		d.errors.Inc()
		return
	}
	d.up.Set(1)

	if !d.initialised {
		d.initialised = true

		d.on = stdGauge("on", "Is the plug on", info)
		d.onTime = stdGauge("onTime", "Cumulative on time", info) // Cannot be a counter because Tapo may reset.
		d.overheated = stdGauge("overheated", "Is the plug overheated", info)

		d.supportsPower = strings.EqualFold("P110", info.Model)
		if d.supportsPower {
			d.currentPower = stdGauge("power", "power (watts)", info)
			d.todayRuntime = stdGauge("today_runtime", "Runtime today (mins)", info)
			d.todayWattHours = stdGauge("today_energy", "Energy today (watt-hours)", info)
		}
	}

	d.on.Set(b2f(info.DeviceOn))
	d.onTime.Set(info.OnTime)
	d.overheated.Set(b2f(info.Overheated))

	if d.supportsPower {
		energy, err := d.session.GetEnergyUsage()
		if err == nil {
			d.todayRuntime.Set(float64(energy.TodayRuntimeMins))
			d.todayWattHours.Set(float64(energy.TodayEnergyWattHours))
			d.currentPower.Set(float64(energy.CurrentPowerMilliWatts) / 1000.0)
		}
	}
}

func (d *Device) Describe(ch chan<- *prometheus.Desc) {
	describe(d.up, ch)
	describe(d.errors, ch)
	describe(d.on, ch)
	describe(d.onTime, ch)
	describe(d.overheated, ch)
	describe(d.currentPower, ch)
	describe(d.todayRuntime, ch)
	describe(d.todayWattHours, ch)
}

func describe(m prometheus.Metric, ch chan<- *prometheus.Desc) {
	if m != nil {
		ch <- m.Desc()
	}
}

func (d *Device) Collect(ch chan<- prometheus.Metric) {
	d.Lock()
	defer d.Unlock()

	collect(d.up, ch)
	collect(d.errors, ch)

	if d.lastWasValid {
		collect(d.on, ch)
		collect(d.onTime, ch)
		collect(d.overheated, ch)
		collect(d.currentPower, ch)
		collect(d.todayRuntime, ch)
		collect(d.todayWattHours, ch)
	}
}

func collect(m prometheus.Collector, ch chan<- prometheus.Metric) {
	if m != nil {
		m.Collect(ch)
	}
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func stdGauge(name string, help string, info *tapo.DeviceInfo) prometheus.Gauge {
	devType := strings.ToLower(info.Avatar)
	if devType == "" {
		devType = info.Model
	}
	nick := info.Nickname
	return prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: subsystem,
		Name:      name,
		Help:      help,
		ConstLabels: prometheus.Labels{
			"model": info.Model,
			"ip":    info.IP,
			"mac":   info.Mac,
			"type":  devType,
			"name":  nick,
		},
	})
}

type Exporter struct {
	mutex   sync.Mutex
	devices map[string]*Device
}

func NewExporter() (*Exporter, error) {

	devices := make(map[string]*Device)
	for _, devAddress := range *tapoDevices {
		dev, err := NewDevice(devAddress)
		if err != nil {
			// Should never happen in practice, even if device is offline.
			return nil, err
		}
		devices[devAddress] = dev
	}

	return &Exporter{
		devices: devices,
	}, nil
}

func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	for _, dev := range e.devices {
		dev.Describe(ch)
	}
}

func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	start := time.Now()

	wg := new(sync.WaitGroup)
	wg.Add(len(e.devices))
	for _, dev := range e.devices {
		go func(dev *Device) {
			defer wg.Done()
			dev.refresh()
			dev.Collect(ch)
		}(dev)
	}
	wg.Wait()

	level.Debug(logger).Log("op", "collect", "time", time.Since(start))
}
