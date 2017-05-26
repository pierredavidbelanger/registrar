# registrar

A Go program that watch Docker events and register / deregister containers as services in Consul.

## usage

```sh
docker run \
    --net host \
    -v /var/run/docker.sock:/var/run/docker.sock \
    pierredavidbelanger/registrar
```

This will connect to Consul on `localhost:8500` (this is why we need `--net host`)
and Docker on `unix:///var/run/docker.sock` (this is why we need `-v /var/run/docker.sock:/var/run/docker.sock`),
then listen for Docker events.

When a container become `"start", "restart", "unpause", "health_status: healthy"` it will be registered as a service in Consul.

When a container become `"die", "oom", "pause", "health_status: unhealthy"` it will be deregistered from Consul.
