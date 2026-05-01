FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN go mod tidy && CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/psp-mock ./cmd

FROM alpine:3.20
RUN adduser -D -u 10001 app
COPY --from=build /out/psp-mock /usr/local/bin/psp-mock
USER app
EXPOSE 9090
ENTRYPOINT ["/usr/local/bin/psp-mock"]
