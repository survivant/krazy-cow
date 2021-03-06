package main

import (
	"crypto/subtle"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/mssola/user_agent"
	"github.com/op/go-logging"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/viper"
	"gitlab.com/cloudowski/krazy-cow/pkg/cow"
	"gitlab.com/cloudowski/krazy-cow/pkg/shepherd"
)

var (
	c              cow.Cow
	cowconf        *viper.Viper
	logger         *logging.Logger
	metricRequests = promauto.NewCounter(prometheus.CounterOpts{
		Name: "cow_requests",
		Help: "The total number of processed requests",
	})
	metricCowMood = promauto.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "cow_mood",
		Help: "Numeric value representing mood of the cow",
	}, getCowMood)
)

func init() {
	c = cow.NewCow()

	// loggers for modules
	logger = logging.MustGetLogger("main")
	cowlogger := logging.MustGetLogger("cow")
	logbackend := logging.NewLogBackend(os.Stderr, "", 0)
	// logformat := logging.MustStringFormatter(`%{time:2006-01-02 15:04:05.9999} %{shortfunc} %{color} %{level} %{message} %{color:reset}`)
	logformat := logging.MustStringFormatter(`%{time:2006-01-02 15:04:05.999} %{module:-7s} %{color} %{level} %{message} %{color:reset}`)

	backendformatter := logging.NewBackendFormatter(logbackend, logformat)
	logbackendleveled := logging.AddModuleLevel(backendformatter)
	logger.SetBackend(logbackendleveled)

	cowlogger.SetBackend(logbackendleveled)
	cow.SetLogger(cowlogger)

	cowversion := version
	if gitCommit != "" {
		cowversion += "-" + gitCommit
	}
	c.SetVersion(cowversion)

	cowconf = viper.New()

	cowconf.SetEnvPrefix("KC")
	cowconf.SetConfigName("defaultconfig") // name of config file (without extension)
	cowconf.AddConfigPath("config/")
	err := cowconf.ReadInConfig() // Find and read the config file
	if err != nil {               // Handle errors reading the config file
		logger.Fatalf("Fatal error config file: %s \n", err)
	}

	cowconf.SetConfigName("cowconfig") // name of config file (without extension)
	cowconf.AddConfigPath("/config/")
	cowconf.AddConfigPath(".")
	cowconf.MergeInConfig()

	cowconf.SetDefault("cow.say", "Mooo")

	cowconf.AutomaticEnv()
	cowconf.SetEnvKeyReplacer(strings.NewReplacer(".", "_")) // replace "." with "_" for nested keys

	loglevel, _ := logging.LogLevel(cowconf.GetString("logging.level"))
	logging.SetLevel(loglevel, "main")
	logging.SetLevel(loglevel, "cow")

	logger.Noticef("🐮 cow %s (%s version %s) initialized", c.Name, APPNAME, version+"-"+gitCommit)

	logger.Debugf("Config: %v", cowconf.AllSettings())

	c.SetMood(cowconf.GetInt("cow.initmood"))
	c.SetSay(cowconf.GetString("cow.say"))

	shepherd.RedisUrl = cowconf.GetString("cow.shepherd.redisurl")

	if cowconf.GetBool("cow.moodchanger.enabled") {
		go c.MoodChanger(cowconf.GetInt("cow.moodchanger.interval"), cowconf.GetInt("cow.moodchanger.change"))
	}
	go c.Grass(cowconf.GetString("cow.pasture.path"), cowconf.GetInt("cow.pasture.interval"))
}

func main() {

	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/", logwrap(c.Say))

	if cowconf.GetBool("http.auth.enabled") {
		logger.Notice("Securing access to /setfree endpoint with basic http authentication")
		user, pass, err := readCreds(cowconf.GetString("http.auth.credentials"))
		// log.Println(user, pass)
		if err != nil {
			logger.Fatal(err)
		}
		http.HandleFunc("/setfree", logwrap(basicAuth(c.SetFree, user, pass, "Secret Access")))
	} else {
		http.HandleFunc("/setfree", logwrap(c.SetFree))
	}

	http.HandleFunc("/healthz", logwrap(c.Healthcheck))

	httpPort := fmt.Sprintf(":%s", cowconf.GetString("http.port"))
	httpsPort := fmt.Sprintf(":%s", cowconf.GetString("http.tls.port"))
	if cowconf.GetBool("http.tls.enabled") {
		logger.Noticef("Starting https version on %s", httpsPort)
		go func() {

			if err := http.ListenAndServeTLS(httpsPort, cowconf.GetString("http.tls.cert"), cowconf.GetString("http.tls.key"), nil); err != nil {
				logger.Fatal(err)
			}

		}()
	}

	logger.Noticef("Starting plain http version on %s", httpPort)

	if err := http.ListenAndServe(httpPort, nil); err != nil {
		logger.Fatal(err)
	}
}

func logwrap(h http.HandlerFunc) http.HandlerFunc {

	return func(w http.ResponseWriter, r *http.Request) {
		c.Requests++
		metricRequests.Inc()
		// ua := r.UserAgent()
		ua := user_agent.New(r.UserAgent())
		browser, _ := ua.Browser()

		if cowconf.GetBool("logging.requests") {
			logger.Infof("%v uri: %v host: %v, user-agent: %s", c.Requests, r.RequestURI, r.RemoteAddr, r.UserAgent())
		}
		shepherd.SendStats(c.Name, fmt.Sprintf("%v %v %v", r.RequestURI, r.RemoteAddr, browser))
		r.Header.Set(cow.HeaderHttpTextClientKey, fmt.Sprintf("%v", isHTTPTextClient(browser)))
		h(w, r)
	}

}

func isHTTPTextClient(useragent string) bool {
	r := regexp.MustCompile("(?i)(curl)|(wget)")
	return r.MatchString(useragent)

}

func basicAuth(handler http.HandlerFunc, username, password, realm string) http.HandlerFunc {

	return func(w http.ResponseWriter, r *http.Request) {

		user, pass, ok := r.BasicAuth()

		if !ok || subtle.ConstantTimeCompare([]byte(user), []byte(username)) != 1 || subtle.ConstantTimeCompare([]byte(pass), []byte(password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="`+realm+`"`)
			w.WriteHeader(401)
			w.Write([]byte("Unauthorized.\n"))
			return
		}

		handler(w, r)
	}
}

func readCreds(file string) (string, string, error) {

	data, err := ioutil.ReadFile(file)
	if err != nil {
		return "", "", fmt.Errorf("Failed to read credentials from %s: %v", file, err)
	}
	creds := regexp.MustCompile(":").Split(strings.TrimSpace(string(data)), 2)

	if len(creds) == 2 {
		return creds[0], creds[1], nil
	}
	return "", "", fmt.Errorf("Failed to read credentials from %s", file)
}

func getCowMood() float64 {
	return float64(c.GetMood())
}
