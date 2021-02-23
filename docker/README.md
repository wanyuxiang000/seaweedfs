# Docker


## Try it out

```bash

wget https://raw.githubusercontent.com/chrislusf/seaweedfs/master/docker/seaweedfs-compose.yml

docker-compose -f seaweedfs-compose.yml -p seaweedfs up

```

## Try latest tip

```bash

wget https://raw.githubusercontent.com/chrislusf/seaweedfs/master/docker/seaweedfs-dev-compose.yml

docker-compose -f seaweedfs-dev-compose.yml -p seaweedfs up

```

## Local Development

```bash
cd $GOPATH/src/github.com/chrislusf/seaweedfs/docker
make
```

## Build and push a multiarch build

Make sure that `docker buildx` is supported (might be an experimental docker feature)
```bash
BUILDER=$(docker buildx create --driver docker-container --use)
docker buildx build --pull --push --platform linux/386,linux/amd64,linux/arm64,linux/arm/v7,linux/arm/v6 . -t chrislusf/seaweedfs
docker buildx stop $BUILDER
```

