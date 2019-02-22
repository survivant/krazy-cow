package main

import (
	"crypto/subtle"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"regexp"
	"strings"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/viper"
	"gitlab.com/cloudowski/krazy-cow/pkg/cow"
	"gitlab.com/cloudowski/krazy-cow/pkg/shepherd"
)

var c cow.Cow
var cowconf *viper.Viper

func init() {
	c = cow.NewCow()

	log.Printf("cow %s (%s version %s) initialized", c.Name, APPNAME, VERSION)

	cowconf = viper.New()

	cowconf.SetEnvPrefix("KC")
	cowconf.SetConfigName("defaultconfig") // name of config file (without extension)
	cowconf.AddConfigPath("config/")
	err := cowconf.ReadInConfig() // Find and read the config file
	if err != nil {               // Handle errors reading the config file
		log.Fatalf("Fatal error config file: %s \n", err)
	}

	cowconf.SetConfigName("cowconfig") // name of config file (without extension)
	cowconf.AddConfigPath("/config/")
	cowconf.AddConfigPath(".")
	cowconf.MergeInConfig()

	cowconf.SetDefault("cow.say", "Mooo")
	cowconf.SetDefault("logging.requests", false)

	cowconf.AutomaticEnv()
	cowconf.SetEnvKeyReplacer(strings.NewReplacer(".", "_")) // replace "." with "_" for nested keys

	log.Printf("Config: %v", cowconf.AllSettings())

	c.SetMood(cowconf.GetInt("cow.initmood"))
	c.SetSay(cowconf.GetString("cow.say"))

	if cowconf.GetBool("cow.moodchanger.enabled") {
		go c.MoodChanger(cowconf.GetInt("cow.moodchanger.interval"), cowconf.GetInt("cow.moodchanger.change"))
	}
	go c.Grass(cowconf.GetString("cow.pasture.path"), cowconf.GetInt("cow.pasture.interval"))
}

func main() {

	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/", logging(c.Say))

	if cowconf.GetBool("http.auth.enabled") {
		log.Println("Securing access to /setfree endpoint with basic http authentication")
		user, pass, err := readCreds(cowconf.GetString("http.auth.credentials"))
		// log.Println(user, pass)
		if err != nil {
			log.Fatal(err)
		}
		http.HandleFunc("/setfree", logging(basicAuth(c.SetFree, user, pass, "Secret Access")))
	} else {
		http.HandleFunc("/setfree", logging(c.SetFree))
	}

	http.HandleFunc("/healthz", logging(c.Healthcheck))

	http_port := fmt.Sprintf(":%s", cowconf.GetString("http.port"))
	https_port := fmt.Sprintf(":%s", cowconf.GetString("http.tls.port"))
	// var err error
	if cowconf.GetBool("http.tls.enabled") {
		log.Printf("Starting https version on %s", https_port)
		go func() {

			if err := http.ListenAndServeTLS(https_port, cowconf.GetString("http.tls.cert"), cowconf.GetString("http.tls.key"), nil); err != nil {
				log.Fatal(err)
			}

		}()
	}
	log.Printf("Starting plain http version on %s", http_port)

	if err := http.ListenAndServe(http_port, nil); err != nil {
		log.Fatal(err)
	}
}

func logging(h http.HandlerFunc) http.HandlerFunc {

	return func(w http.ResponseWriter, r *http.Request) {
		c.Requests++
		ua := r.UserAgent()

		if cowconf.GetBool("logging.requests") {
			log.Printf("%v uri: %v host: %v, user-agent: %s", c.Requests, r.RequestURI, r.RemoteAddr, ua)
		}
		shepherd.SendStats(c.Name, fmt.Sprintf("%v %v %v", r.RequestURI, r.RemoteAddr, ua))
		h(w, r)
	}

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
	} else {
		return "", "", fmt.Errorf("Failed to read credentials from %s", file)
	}
}
