# Build binary
FROM golang:alpine AS builder

RUN apk update 
RUN apk add --no-cache git

WORKDIR $GOPATH/src/github.com/caesium-dev/caesium
COPY . .

RUN go get -d -v
RUN go build -o /go/bin/caesium

# Package for lightweight deployment
FROM scratch

COPY --from=builder /go/bin/caesium /go/bin/caesium

ENTRYPOINT ["/go/bin/caesium"]