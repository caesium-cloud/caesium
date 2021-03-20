# Build binary
FROM golang:alpine AS builder

RUN apk update 
RUN apk add --no-cache git g++

WORKDIR $GOPATH/src/github.com/caesium-cloud/caesium
COPY . .

RUN go get -d -v
RUN GOOS=linux GOARCH=amd64 go build -o /go/bin/caesium

# Package for lightweight deployment
FROM scratch

COPY --from=builder /go/bin/caesium /go/bin/caesium

ENTRYPOINT ["/go/bin/caesium"]
