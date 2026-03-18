FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/symphony ./cmd/symphony

FROM node:22-bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash \
    ca-certificates \
    git \
    openssh-client \
  && rm -rf /var/lib/apt/lists/*
WORKDIR /work
COPY --from=build /out/symphony /usr/local/bin/symphony
ENTRYPOINT ["symphony"]
