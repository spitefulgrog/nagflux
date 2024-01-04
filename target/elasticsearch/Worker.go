package elasticsearch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/spitefulgrog/nagflux/collector"
	"github.com/spitefulgrog/nagflux/helper"
	"github.com/spitefulgrog/nagflux/logging"
	"github.com/spitefulgrog/nagflux/statistics"
	"github.com/kdar/factorlog"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

//Worker reads data from the queue and sends them to the influxdb.
type Worker struct {
	workerID     int
	quit         chan bool
	quitInternal chan bool
	jobs         chan collector.Printable
	connection   string
	dumpFile     string
	log          *factorlog.FactorLog
	version      string
	connector    *Connector
	httpClient   http.Client
	IsRunning    bool
	index        string
	promServer   statistics.PrometheusServer
}

const dataTimeout = time.Duration(20) * time.Second

var errorInterrupted = errors.New("Got interrupted")
var errorBadRequest = errors.New("400 Bad Request")
var errorHTTPClient = errors.New("Http Client got an error")
var errorFailedToSend = errors.New("Could not send data")
var error500 = errors.New("Error 500")

//WorkerGenerator generates a new Worker and starts it.
func WorkerGenerator(jobs chan collector.Printable, connection, index, dumpFile, version string, connector *Connector) func(workerId int) *Worker {
	return func(workerId int) *Worker {
		worker := &Worker{
			workerId, make(chan bool),
			make(chan bool, 1), jobs,
			connection, dumpFile,
			logging.GetLogger(), version,
			connector, http.Client{}, true, index,
			statistics.GetPrometheusServer()}
		go worker.run()
		return worker
	}
}

//Stop stops the worker
func (worker *Worker) Stop() {
	worker.quitInternal <- true
	worker.quit <- true
	<-worker.quit
	worker.IsRunning = false
	worker.log.Debug("InfluxWorker stopped")
}

//Tries to send data all the time.
func (worker Worker) run() {
	var queries []collector.Printable
	var query collector.Printable
	for {
		if worker.connector.IsAlive() {
			if worker.connector.DatabaseExists() {
				select {
				case <-worker.quit:
					worker.log.Debug("InfluxWorker quitting...")
					worker.sendBuffer(queries)
					worker.quit <- true
					return
				case query = <-worker.jobs:
					queries = append(queries, query)
					if len(queries) == 10000 {
						//9000 ~= 2,4 MB
						worker.sendBuffer(queries)
						queries = queries[:0]
					}
				case <-time.After(dataTimeout):
					worker.sendBuffer(queries)
					queries = queries[:0]
				}
			} else {
				//Test Database
				worker.connector.TestTemplateExists()
				worker.log.Critical("Database does not exists, waiting for the end to come")
				if worker.waitForExternalQuit() {
					return
				}
			}
		} else {
			//Test Influxdb
			worker.connector.TestIfIsAlive()
			worker.log.Critical("InfluxDB is not running, waiting for the end to come")
			if worker.waitForExternalQuit() {
				return
			}
		}
	}
}

//Checks if a external quit signal arrives.
func (worker Worker) waitForExternalQuit() bool {
	select {
	case <-worker.quit:
		worker.quit <- true
		return true
	case <-time.After(time.Duration(30) * time.Second):
		return false
	}
}

//Sends the given queries to the influxdb.
func (worker Worker) sendBuffer(queries []collector.Printable) {
	if len(queries) == 0 {
		return
	}

	var lineQueries []string
	for _, query := range queries {
		cast, castErr := worker.castJobToString(query)
		if castErr == nil {
			lineQueries = append(lineQueries, cast)
		}
	}

	var dataToSend []byte
	for _, lineQuery := range lineQueries {
		dataToSend = append(dataToSend, []byte(lineQuery)...)
	}

	startTime := time.Now()
	sendErr := worker.sendData([]byte(dataToSend), true)
	if sendErr != nil {
		for i := 0; i < 3; i++ {
			switch sendErr {
			case errorBadRequest:
				//Maybe just a few queries are wrong, so send them one by one and find the bad one
				var badQueries []string
				for _, lineQuery := range lineQueries {
					queryErr := worker.sendData([]byte(lineQuery), false)
					if queryErr != nil {
						badQueries = append(badQueries, lineQuery)
					}
				}
				worker.dumpErrorQueries("\n\nOne of the values is not clean..\n", badQueries)
				sendErr = nil
			case nil:
				//Single point of exit
				break
			default:
				if err := worker.waitForQuitOrGoOn(); err != nil {
					//No error handling, because it's time to terminate
					worker.dumpRemainingQueries(lineQueries)
					sendErr = nil
				}
				//Resend Data
				sendErr = worker.sendData([]byte(dataToSend), true)
			}
		}
		if sendErr != nil {
			//if there is still an error dump the queries and go on
			worker.dumpErrorQueries("\n\n"+sendErr.Error()+"\n", lineQueries)
		}

	}
	worker.promServer.BytesSend.WithLabelValues("Elasticsearch").Add(float64(len(lineQueries)))
	worker.promServer.SendDuration.WithLabelValues("Elasticsearch").Add(float64(time.Since(startTime).Seconds() * 1000))
}

//Writes the bad queries to a dumpfile.
func (worker Worker) dumpErrorQueries(messageForLog string, errorQueries []string) {
	errorFile := worker.dumpFile + "-errors"
	worker.log.Warnf("Dumping queries with errors to: %s", errorFile)
	errorQueries = append([]string{messageForLog}, errorQueries...)
	worker.dumpQueries(errorFile, errorQueries)
}

var mutex = &sync.Mutex{}

//Dumps the remaining queries if a quit signal arises.
func (worker Worker) dumpRemainingQueries(remainingQueries []string) {
	mutex.Lock()
	worker.log.Debugf("Global queue %d own queue %d", len(worker.jobs), len(remainingQueries))
	if len(worker.jobs) != 0 || len(remainingQueries) != 0 {
		worker.log.Debug("Saving queries to disk")

		remainingQueries = append(remainingQueries, worker.readQueriesFromQueue()...)

		worker.log.Debugf("dumping %d queries", len(remainingQueries))
		worker.dumpQueries(worker.dumpFile, remainingQueries)
	}
	mutex.Unlock()
}

//Reads the queries from the global queue and returns them as string.
func (worker Worker) readQueriesFromQueue() []string {
	var queries []string
	var query collector.Printable
	stop := false
	for !stop {
		select {
		case query = <-worker.jobs:
			cast, err := worker.castJobToString(query)
			if err == nil {
				queries = append(queries, cast)
			}
		case <-time.After(time.Duration(200) * time.Millisecond):
			stop = true
		}
	}
	return queries
}

//sends the raw data to influxdb and returns an err if given.
func (worker Worker) sendData(rawData []byte, log bool) error {
	worker.log.Debug(string(rawData))
	req, err := http.NewRequest("POST", worker.connection, bytes.NewBuffer(rawData))
	if err != nil {
		worker.log.Warn(err)
	}
	req.Header.Set("User-Agent", "Nagflux")
	resp, err := worker.httpClient.Do(req)
	if err != nil {
		worker.log.Warn(err)
		return errorHTTPClient
	}
	defer resp.Body.Close()
	jsonSrc, _ := ioutil.ReadAll(resp.Body)
	var result JSONResult
	json.Unmarshal(jsonSrc, &result)
	if !result.Errors {
		//OK
		return nil
	}
	//Bad Request
	if log {
		worker.printErrors(result, rawData)
		worker.log.Warn(result.Items)
	}
	return nil //TODO: return error
}

func (worker Worker) printErrors(result JSONResult, rawData []byte) {
	errors := []map[int]string{{}}
	for index, item := range result.Items {
		if item.Create.Status != 201 {
			errors = append(errors, map[int]string{index * 2: strings.Replace(item.Create.Error.CausedBy.Reason, "\n", "", -1) + "\n"})
		}
	}

	ioutil.WriteFile(worker.dumpFile+".currupt.json", rawData, 0644)
	ioutil.WriteFile(worker.dumpFile+".error.json", []byte(fmt.Sprintf("%v", errors)), 0644)
	panic("")
}

//Logs a http response to warn.
func (worker Worker) logHTTPResponse(resp *http.Response) {
	body, _ := ioutil.ReadAll(resp.Body)
	worker.log.Warnf("Influx status: %s - %s", resp.Status, string(body))
}

//Waits on an internal quit signal.
func (worker Worker) waitForQuitOrGoOn() error {
	select {
	//Got stop signal
	case <-worker.quitInternal:
		worker.log.Debug("Received quit")
		worker.quitInternal <- true
		return errorInterrupted
	//Timeout and retry
	case <-time.After(time.Duration(10) * time.Second):
		return nil
	}
}

//Writes queries to a dumpfile.
func (worker Worker) dumpQueries(filename string, queries []string) {
	if _, err := os.Stat(filename); os.IsNotExist(err) {
		if _, err := os.Create(filename); err != nil {
			worker.log.Critical(err)
		}
	}
	if f, err := os.OpenFile(filename, os.O_APPEND|os.O_WRONLY, 0600); err != nil {
		worker.log.Critical(err)
	} else {
		defer f.Close()
		for _, query := range queries {
			if _, err = f.WriteString(query); err != nil {
				worker.log.Critical(err)
			}
		}
	}
}

//Converts an collector.Printable to a string.
func (worker Worker) castJobToString(job collector.Printable) (string, error) {
	var result string
	var err error

	if helper.VersionOrdinal(worker.version) >= helper.VersionOrdinal("2.0") {
		result = job.PrintForElasticsearch(worker.version, worker.index)
	} else {
		worker.log.Fatalf("This elasticsearch version [%s] given in the config is not supported", worker.version)
		err = errors.New("This elasticsearch version given in the config is not supported")
	}

	if len(result) > 1 && result[len(result)-1:] != "\n" {
		result += "\n"
	}
	return result, err
}
