package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
)

var gotAuth = false
var DOLLAR_INT_REGEXP = regexp.MustCompile("\\$([0-9]+)\r")
var REDIS_KEY_NAME = "belugacdn"
var ASCII_CR = byte(13)
var ASCII_LF = byte(10)
var INTEGER_REGEXP = regexp.MustCompile("^[0-9]+$")
var FLOAT_REGEXP = regexp.MustCompile("^[0-9]+\\.[0-9]+$")
var FIELD_KEY_REGEXP = regexp.MustCompile("^[a-z_]+$")

type Config struct {
	ListenPort          string
	ExpectedPassword    string
	InfluxdbHost        string
	InfluxdbPort        string
	InfluxdbDatabase    string
	InfluxdbMeasurement string
}

func awaitAuthCommand(reader *bufio.Reader, conn net.Conn, expectedPassword string) {
	log.Println("Awaiting AUTH command...")
	expect(reader, "*2")                                      // AUTH command has 2 parts
	expect(reader, "$4")                                      // part 1 has 4 chars
	expect(reader, "AUTH")                                    // part 1 is the word AUTH
	expect(reader, fmt.Sprintf("$%d", len(expectedPassword))) // part 2 has n chars
	expect(reader, expectedPassword)                          // part 2 is the password

	_, err := conn.Write([]byte("+OK\r\n"))
	if err != nil {
		log.Fatal(err)
	}
}

func awaitLpushCommand(reader *bufio.Reader, conn net.Conn, influxdbClient *http.Client,
	config *Config) {

	log.Println("Awaiting LPUSH command...")
	expect(reader, "*3")                                    // LPUSH command has 3 parts
	expect(reader, "$5")                                    // part 1 has 5 chars
	expect(reader, "LPUSH")                                 // part 1 is the word LPUSH
	expect(reader, fmt.Sprintf("$%d", len(REDIS_KEY_NAME))) // part 2 has n chars
	expect(reader, REDIS_KEY_NAME)                          // part 2 is the key

	var upcomingStringLength = expectDollarInt(reader)
	log.Printf("Got $%d", upcomingStringLength)

	var logJson = make([]byte, upcomingStringLength)
	var numBytesRead, err = reader.Read(logJson)
	if err != nil {
		log.Fatal(err)
	}
	if numBytesRead != upcomingStringLength {
		log.Fatalf("Expected %d bytes but read %d", upcomingStringLength, numBytesRead)
	}
	log.Printf("Read log: %s", string(logJson))

	cr, err := reader.ReadByte()
	if cr != ASCII_CR {
		log.Fatalf("Expected CR but got %v", cr)
	}
	lf, err := reader.ReadByte()
	if lf != ASCII_LF {
		log.Fatalf("Expected LF but got %v", lf)
	}

	keyValues := parseLogJson(logJson)
	log.Printf("keyValues: %v", keyValues)
	insertIntoInfluxDb(keyValues, influxdbClient, config)

	_, err = conn.Write([]byte(":1\r\n")) // say the length of the list is 1 long
	if err != nil {
		log.Fatal(err)
	}
}

func parseLogJson(logJson []byte) map[string]interface{} {
	parsed := &map[string]interface{}{}
	err := json.Unmarshal(logJson, parsed)
	if err != nil {
		log.Fatal(err)
	}
	return *parsed
}

func insertIntoInfluxDb(keyValues map[string]interface{}, influxdbClient *http.Client,
	config *Config) {

	var query bytes.Buffer

	query.WriteString(config.InfluxdbMeasurement)

	var isFirstKey = true
	for key, value := range keyValues {
		if key != "time" {
			if isFirstKey == true {
				query.WriteString(" ")
				isFirstKey = false
			} else {
				query.WriteString(",")
			}

			if !FIELD_KEY_REGEXP.MatchString(key) {
				log.Fatalf("Unexpected characters in field key '%s'", key)
			}
			query.WriteString(key)
			query.WriteString("=")

			valueString, ok := value.(string)
			if !ok {
				log.Fatalf("Don't know how to handle value %v type %T", value, value)
			}

			// Don't consider key=status to be an integer
			if key == "response_size" || key == "header_size" {
				if !INTEGER_REGEXP.MatchString(valueString) {
					log.Fatalf("Expected key=%s to be integer value but was '%s'", key, valueString)
				}
				query.WriteString(valueString)
				query.WriteString("i") // mark as integer
			} else if key == "duration" {
				if !FLOAT_REGEXP.MatchString(valueString) {
					log.Fatalf("Expected key=%s to be float value but was '%s'", key, valueString)
				}
				query.WriteString(valueString)
			} else {
				query.WriteString("\"")
				query.WriteString(strings.Replace(valueString, "\"", "\\\"", -1))
				query.WriteString("\"")
			}
		}
	}

	timestamp := keyValues["time"].(string)
	if !INTEGER_REGEXP.MatchString(timestamp) {
		log.Fatalf("Unexpected characters in timestamp '%s'", timestamp)
	}
	query.WriteString(" ")
	query.WriteString(timestamp)

	log.Printf("Query is %s", query.String())
	url := "http://" + config.InfluxdbHost + ":" + config.InfluxdbPort +
		"/write?db=belugacdn&precision=s"
	resp, err := influxdbClient.Post(url, "application/x-www-form-urlencoded", &query)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()

	fmt.Println("response Status:", resp.Status)
	if resp.StatusCode != 200 && resp.StatusCode != 204 {
		log.Fatalf("Bad status %s from POST %s", resp.Status, url)
	}
	fmt.Println("response Headers:", resp.Header)
	body, _ := ioutil.ReadAll(resp.Body)
	fmt.Println("response Body:", string(body))
}

func handleConnection(conn net.Conn, config *Config, influxdbClient *http.Client) {
	log.Println("Handling new connection...")

	// Close connection when this function ends
	defer func() {
		log.Println("Closing connection...")
		conn.Close()
	}()

	reader := bufio.NewReader(conn)

	// Set a deadline for reading. Read operation will fail if no data
	// is received after deadline.
	// timeoutDuration := 5 * time.Second
	// conn.SetReadDeadline(time.Now().Add(timeoutDuration))

	awaitAuthCommand(reader, conn, config.ExpectedPassword)

	for {
		awaitLpushCommand(reader, conn, influxdbClient, config)
	}
}

func expect(reader *bufio.Reader, expected string) {
	bytes, err := reader.ReadBytes('\n')
	if err != nil {
		log.Fatal(err)
	}

	if strings.ToUpper(strings.TrimSpace(string(bytes))) != strings.ToUpper(expected) {
		log.Fatalf("Expected %s but got %s", expected, bytes)
	}
}

func expectDollarInt(reader *bufio.Reader) int {
	bytes, err := reader.ReadBytes('\n')
	if err != nil {
		log.Fatal(err)
	}
	var match = DOLLAR_INT_REGEXP.FindStringSubmatch(string(bytes))
	log.Printf("Got match = %s", match[1])

	i, err := strconv.Atoi(match[1])
	if err != nil {
		log.Fatal(err)
	}

	return i
}

func startRedisListener(config *Config, influxdbClient *http.Client) {
	listener, err := net.Listen("tcp", ":"+config.ListenPort)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Listening on port %s...", config.ListenPort)

	defer func() {
		listener.Close()
		log.Println("Listener closed")
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go handleConnection(conn, config, influxdbClient)
	}
}

func main() {
	if len(os.Args) < 1+1 {
		log.Fatal("First arg should be config.json")
	}

	configJson, err := ioutil.ReadFile(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}

	var config = &Config{}
	if err := json.Unmarshal(configJson, config); err != nil {
		panic(err)
	}

	influxdbClient := &http.Client{}
	//form := url.Values{}
	//form.Set("q", "CREATE DATABASE "+INFLUXDB_DATABASE_NAME)
	//resp, err := influxdbClient.PostForm(url, form)
	url := "http://" + config.InfluxdbHost + ":" + config.InfluxdbPort +
		"/query?q=" + url.QueryEscape("CREATE DATABASE "+config.InfluxdbDatabase)
	resp, err := influxdbClient.Get(url)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()

	fmt.Println("response Status:", resp.Status)
	if resp.StatusCode != 200 {
		log.Fatalf("Bad status %s from %s", resp.Status, url)
	}
	fmt.Println("response Headers:", resp.Header)
	body, _ := ioutil.ReadAll(resp.Body)
	fmt.Println("response Body:", string(body))

	startRedisListener(config, influxdbClient)
}
