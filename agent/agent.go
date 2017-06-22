package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Dataman-Cloud/swan/agent/janitor"
	"github.com/Dataman-Cloud/swan/agent/janitor/upstream"
	"github.com/Dataman-Cloud/swan/agent/nameserver"
	"github.com/Dataman-Cloud/swan/config"
	"github.com/Dataman-Cloud/swan/types"
	"github.com/Dataman-Cloud/swan/utils"

	"github.com/Sirupsen/logrus"
)

const REJOIN_BACKOFF = 3 * time.Second
const SSE_DATA_PREFIX = "data:"
const SSE_EVENT_PREFIX = "event:"
const SSE_BLANK_LINE = ""

// Agent struct
type Agent struct {
	Resolver   *nameserver.Resolver
	Janitor    *janitor.JanitorServer
	HTTPServer *HTTPServer
	Config     config.AgentConfig
	eventCh    chan *event
}

type event struct {
	name    string
	payload []byte
}

// New agent func
func New(agentConf config.AgentConfig) *Agent {
	agent := &Agent{
		Config:   agentConf,
		Resolver: nameserver.NewResolver(&agentConf.DNS),
		Janitor:  janitor.NewJanitorServer(&agentConf.Janitor),
		eventCh:  make(chan *event, 1024),
	}
	agent.HTTPServer = NewHTTPServer(agentConf.ListenAddr, agent)
	return agent
}

// StartAndJoin func
func (agent *Agent) StartAndJoin() error {
	errCh := make(chan error)

	go func() {
		err := agent.Resolver.Start()
		if err != nil {
			errCh <- err
		}
		logrus.Warnln("resolver quit, error:", err)
	}()

	go func() {
		err := agent.Janitor.Start()
		if err != nil {
			errCh <- err
		}
		logrus.Warnln("janitor quit, error:", err)
	}()

	go func() {
		err := agent.HTTPServer.Start()
		if err != nil {
			errCh <- err
		}
		logrus.Warnln("http server quit, error:", err)
	}()

	go agent.watchEvents()
	go agent.dispatchEvents()

	return <-errCh
}

// watchEvents establish a connection to swan master's stream events endpoint
// and broadcast received events
func (agent *Agent) watchEvents() {
	for {
		leaderAddr, err := agent.detectManagerLeader()
		if err != nil {
			logrus.Errorf("detect manager leader got error: %v, retry ...", err)
			time.Sleep(REJOIN_BACKOFF)
			continue
		}
		logrus.Printf("detected manager addr %s, listening on events ...", leaderAddr)

		err = agent.watchManagerEvents(leaderAddr)
		if err != nil {
			logrus.Errorf("watch manager events got error: %v, retry ...", err)
			time.Sleep(REJOIN_BACKOFF)
		}
	}
}

// dispatchEvents dispatch received events to dns & proxy goroutines
func (agent *Agent) dispatchEvents() {
	// send proxy info to dns proxy listener
	agent.Resolver.EmitChange(&nameserver.RecordChangeEvent{
		Change: "add",
		Record: nameserver.Record{
			Type:    nameserver.A,
			Ip:      agent.Config.Janitor.AdvertiseIP,
			IsProxy: true,
		},
	})

	for event := range agent.eventCh {
		var taskEvent types.TaskEvent
		err := json.Unmarshal(event.payload, &taskEvent)
		if err != nil {
			logrus.Errorf("unmarshal taskInfoEvent go error: %s", err.Error())
			continue
		}

		if taskEvent.GatewayEnabled {
			agent.Janitor.EmitEvent(genJanitorBackendEvent(
				event.name, &taskEvent))
		}

		// Resolver only recongnize these two events
		if event.name == types.EventTypeTaskHealthy ||
			event.name == types.EventTypeTaskUnhealthy {
			agent.Resolver.EmitChange(recordChangeEventFromTaskInfoEvent(
				event.name, &taskEvent))
		}
	}
}

// todo
func (agent *Agent) detectManagerLeader() (string, error) {
	for _, managerAddr := range agent.Config.JoinAddrs {
		if _, err := http.Get(managerAddr + "/ping"); err != nil {
			logrus.Infof("detect manager %s error %v", managerAddr, err)
			continue
		}

		return managerAddr, nil
	}

	return "", errors.New("try join all swan managers failed")
}

func (agent *Agent) watchManagerEvents(leaderAddr string) error {
	eventsDoesMatter := []string{
		types.EventTypeTaskUnhealthy,
		types.EventTypeTaskHealthy,
		types.EventTypeTaskWeightChange,
	}

	eventsPath := fmt.Sprintf("http://%s/events?catchUp=true", leaderAddr)
	resp, err := http.Get(eventsPath)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}

		// skip blank line
		if line == SSE_BLANK_LINE {
			continue
		}

		if strings.HasPrefix(line, SSE_EVENT_PREFIX) {
			eventType := strings.TrimSpace(line[len(SSE_EVENT_PREFIX):len(line)])
			if !utils.SliceContains(eventsDoesMatter, eventType) {
				continue
			}

			// read next line of stream
			line, err := reader.ReadString('\n')
			if err != nil {
				return err
			}
			// if line is not data section
			if !strings.HasPrefix(line, SSE_DATA_PREFIX) {
				continue
			}

			agent.eventCh <- &event{
				name:    eventType,
				payload: []byte(line[len(SSE_DATA_PREFIX):len(line)]),
			}
		}
	}
}

func recordChangeEventFromTaskInfoEvent(eventType string, taskInfoEvent *types.TaskEvent) *nameserver.RecordChangeEvent {
	resolverEvent := &nameserver.RecordChangeEvent{}
	if eventType == types.EventTypeTaskHealthy {
		resolverEvent.Change = "add"
	} else {
		resolverEvent.Change = "del"
	}

	// port & type
	resolverEvent.Type = nameserver.SRV ^ nameserver.A
	resolverEvent.Port = fmt.Sprintf("%d", taskInfoEvent.Port)
	// the rest
	resolverEvent.AppName = taskInfoEvent.AppID
	resolverEvent.Ip = taskInfoEvent.IP
	resolverEvent.Weight = taskInfoEvent.Weight

	return resolverEvent
}

func genJanitorBackendEvent(eventType string, taskInfoEvent *types.TaskEvent) *upstream.BackendEvent {
	var (
		act string

		// upstream
		ups    = taskInfoEvent.AppID
		alias  = "" // TODO
		listen = "" // TODO

		// backend
		backend = taskInfoEvent.TaskID
		ip      = taskInfoEvent.IP
		port    = taskInfoEvent.Port
		weight  = taskInfoEvent.Weight
		version = taskInfoEvent.VersionID
	)

	switch eventType {
	case types.EventTypeTaskHealthy:
		act = "add"
	case types.EventTypeTaskUnhealthy:
		act = "del"
	case types.EventTypeTaskWeightChange:
		act = "change"
	default:
		return nil
	}

	return upstream.BuildBackendEvent(act, ups, alias, listen, backend, ip, version, port, weight)
}