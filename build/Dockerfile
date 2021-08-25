FROM golang:alpine AS builder

# Update & install OS dependencies
RUN apk update 
RUN apk add --no-cache git g++

# Download Go dependencies
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download

# Build binary
COPY . .
ENV CGO_ENABLED=1 \
    GOOS=linux \
    GOARCH=amd64
RUN go build

# Link for CGO
WORKDIR /dist
RUN cp /build/caesium .
RUN ldd caesium | tr -s '[:blank:]' '\n' | grep '^/' | \
    xargs -I % sh -c 'mkdir -p $(dirname ./%); cp % ./%;'

# Package for lightweight deployment
FROM alpine
COPY --chown=0:0 --from=builder /dist /bin/
ENTRYPOINT ["/bin/caesium"]