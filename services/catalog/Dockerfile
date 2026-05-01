FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN go mod tidy && CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/catalog ./cmd

FROM alpine:3.20
RUN adduser -D -u 10001 app
COPY --from=build /out/catalog /usr/local/bin/catalog
USER app
EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/catalog"]
