FROM golang:1.27-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w -X app/pkg/integration.Version=${VERSION}" -o /out/lws .

FROM scratch
COPY --from=build /out/lws /lws
COPY configs/config.json /etc/lws/config.json
COPY www/ /srv/www/
USER 65534:65534
EXPOSE 8080 9090
ENTRYPOINT ["/lws"]
CMD ["-config", "/etc/lws/config.json"]
