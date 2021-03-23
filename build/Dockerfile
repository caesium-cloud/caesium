FROM golang:alpine AS builder

# Update & install OS dependencies
RUN apk update 
RUN apk add --no-cache git g++

# Download Go dependencies
WORKDIR $GOPATH/src/github.com/caesium-cloud/caesium
COPY go.mod go.sum ./
RUN go mod download

# Build binary
COPY . .
RUN GOOS=linux GOARCH=amd64 go build -o /go/bin/caesium

# Package for lightweight deployment
FROM scratch
COPY --from=builder /go/bin/caesium /go/bin/caesium
ENTRYPOINT ["/go/bin/caesium"]