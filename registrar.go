package main

import (
	dockerapi "github.com/fsouza/go-dockerclient"
	consulapi "github.com/hashicorp/consul/api"
	log "github.com/sirupsen/logrus"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Registry struct {
	mutex      sync.RWMutex
	serviceIds map[string][]string
	ttr        time.Duration
	ttl        time.Duration
	ttd        time.Duration
	docker     *dockerapi.Client
	consul     *consulapi.Client
}

func getDurationFromEnv(key string, defaultValue time.Duration) time.Duration {
	if s := os.Getenv(key); s != "" {
		if d, err := time.ParseDuration(s); err != nil {
			return d
		}
	}
	return defaultValue
}

func NewRegistry() *Registry {

	registry := &Registry{}

	registry.serviceIds = make(map[string][]string)

	registry.ttr = getDurationFromEnv("REGISTRAR_TTR", 45*time.Second)
	registry.ttl = getDurationFromEnv("REGISTRAR_TTL", 1*time.Minute)
	registry.ttd = getDurationFromEnv("REGISTRAR_TTD", 5*time.Minute)

	log.Debug("Connect to Consul")
	consul, err := consulapi.NewClient(consulapi.DefaultConfig())
	if err != nil {
		log.Fatal("Can not create a Consul API client: ", err)
	}
	for {
		leader, err := consul.Status().Leader()
		if err != nil {
			log.Warn("Can not contact a Consul leader: ", err)
			time.Sleep(time.Second)
		} else if leader == "" {
			log.Warn("Invalid Consul leader: ", leader)
			time.Sleep(time.Second)
		} else {
			log.Debug("Connected to Consul")
			log.Debugf("\tLeader: %s", leader)
			break
		}
	}
	registry.consul = consul

	log.Debug("Connect to Docker")
	docker, err := dockerapi.NewClientFromEnv()
	if err != nil {
		log.Fatal("Unable to create Docker client: ", err)
	}
	env, err := docker.Version()
	if err != nil {
		log.Fatal("Unable to get Docker version: ", err)
	}
	log.Debug("Connected to Docker")
	for key, value := range env.Map() {
		log.Debugf("\t%s: %s", key, value)
	}
	registry.docker = docker

	return registry
}

func (registry *Registry) Run() {

	ticker := time.NewTicker(registry.ttr)

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	events := make(chan *dockerapi.APIEvents, 10)
	registry.docker.AddEventListener(events)

	listContainersOptions := dockerapi.ListContainersOptions{}
	containers, err := registry.docker.ListContainers(listContainersOptions)
	if err != nil {
		log.Fatal("Unable to list Docker containers: ", err)
	}
	for _, container := range containers {
		go registry.Register(container.ID)
	}

	log.Debug("Listen for Docker events")
	for {
		select {
		case <-interrupt:
			registry.UnregisterAll()
			return
		case <-ticker.C:
			go func() {
				err := registry.Refresh()
				if err != nil {
					log.Errorf("Error while Refreshing: %v", err)
				}
			}()
		case event := <-events:
			if event.Type != "container" {
				continue
			}
			log.Debugf("event: %s", event.Action)
			switch event.Action {
			case "start", "restart", "unpause", "health_status: healthy":
				go func() {
					err := registry.Register(event.ID)
					if err != nil {
						log.Errorf("Error while Registering %q: %v", event.ID, err)
					}
				}()
			case "die", "oom", "pause", "health_status: unhealthy":
				go func() {
					err := registry.Unregister(event.ID)
					if err != nil {
						log.Errorf("Error while Unregistering %q: %v", event.ID, err)
					}
				}()
			}
		}
	}
}

func (registry *Registry) Register(containerId string) error {

	container, err := registry.docker.InspectContainer(containerId)
	if err != nil {
		return err
	}

	if container.State.Health.Status != "" && container.State.Health.Status != "healthy" {
		return nil
	}

	imageParts := strings.Split(container.Config.Image, "/")
	imageName := strings.Split(imageParts[len(imageParts)-1], ":")[0]

	for containerPortAndProto, bindings := range container.NetworkSettings.Ports {

		containerPort := containerPortAndProto.Port()

		registrarPortEnabled := container.Config.Labels["registrar."+containerPort+".enabled"]
		if registrarPortEnabled == "true" {

			registrarPortName := container.Config.Labels["registrar."+containerPort+".name"]

			serviceTags := make([]string, 0)
			tagPrefix := "registrar."+containerPort+".tags."
			for label, value := range container.Config.Labels {
				if strings.HasPrefix(label, tagPrefix) {
					if tag := strings.TrimPrefix(label, tagPrefix); tag != "" {
						serviceTags = append(serviceTags, tag+"="+value)
					}
				}
			}

			for _, binding := range bindings {

				if hostPort, err := strconv.Atoi(binding.HostPort); err == nil {

					serviceId := containerId + ":" + binding.HostPort

					var serviceName string
					if registrarPortName != "" {
						serviceName = registrarPortName
					} else {
						if len(container.NetworkSettings.Ports) == 1 {
							serviceName = imageName
						} else {
							serviceName = imageName + ":" + containerPort
						}
					}

					serviceRegistration := &consulapi.AgentServiceRegistration{
						ID:   serviceId,
						Name: serviceName,
						Port: hostPort,
						Tags: serviceTags,
						Check: &consulapi.AgentServiceCheck{
							TTL:    registry.ttl.String(),
							Status: "passing",
							DeregisterCriticalServiceAfter: registry.ttd.String(),
						},
					}

					log.Infof("register service %s (%s:%d)", serviceId, serviceRegistration.Name, serviceRegistration.Port)
					err = registry.consul.Agent().ServiceRegister(serviceRegistration)
					if err != nil {
						log.Warnf("Unable to register service %s: %s", serviceId, err)
					} else {
						registry.mutex.Lock()
						registry.serviceIds[containerId] = append(registry.serviceIds[containerId], serviceId)
						registry.mutex.Unlock()
					}
				}
			}
		}
	}

	return nil
}

func (registry *Registry) Refresh() error {

	done := make(chan bool)

	count := 0

	registry.mutex.RLock()

	for _, serviceIds := range registry.serviceIds {
		for _, serviceId := range serviceIds {
			s := "service:" + serviceId
			go func() {
				log.Debugf("pass TTL for check %s", s)
				err := registry.consul.Agent().PassTTL(s, time.Now().Format(time.RFC3339))
				if err != nil {
					log.Warnf("Unable to pass TTL for %s: %s", s, err)
				}
				done <- true
			}()
			count++
		}
	}

	registry.mutex.RUnlock()

	for i := 0; i < count; i++ {
		<-done
	}

	return nil
}

func (registry *Registry) UnregisterAll() error {

	done := make(chan bool)

	count := 0

	registry.mutex.Lock()

	for containerId := range registry.serviceIds {
		s := containerId
		go func() {
			registry.unregisterNoLock(s)
			done <- true
		}()
		count++
	}

	registry.mutex.Unlock()

	for i := 0; i < count; i++ {
		<-done
	}

	return nil
}

func (registry *Registry) Unregister(containerId string) error {

	registry.mutex.Lock()

	defer registry.mutex.Unlock()

	return registry.unregisterNoLock(containerId)
}

func (registry *Registry) unregisterNoLock(containerId string) error {

	log.Debugf("unregister container %s", containerId)

	done := make(chan bool)

	serviceIds := registry.serviceIds[containerId]

	count := 0

	for _, serviceId := range serviceIds {
		s := serviceId
		go func() {
			log.Infof("unregister service %s", s)
			err := registry.consul.Agent().ServiceDeregister(s)
			if err != nil {
				log.Warnf("Unable to deregister %s: %s", s, err)
			}
			done <- true
		}()
		count++
	}

	delete(registry.serviceIds, containerId)

	for i := 0; i < count; i++ {
		<-done
	}

	return nil
}

func main() {

	if logLevelEnv := os.Getenv("LOG_LEVEL"); logLevelEnv != "" {
		logLevel, err := log.ParseLevel(logLevelEnv)
		if err != nil {
			log.Fatal("Invalid log level: ", err)
		}
		log.SetLevel(logLevel)
	}

	registry := NewRegistry()

	registry.Run()
}
