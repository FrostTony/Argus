FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Set only by a tagged build; Go records the commit by itself.
ARG VERSION=
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w ${VERSION:+-X github.com/tonyamdfrost-cmd/Argus/internal/app.Version=$VERSION}" \
    -o /argus ./cmd/argus

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 argus
COPY --from=build /argus /usr/local/bin/argus

# Unprivileged ICMP; without this, icmp probes need icmp.privileged and NET_RAW.
RUN echo "net.ipv4.ping_group_range = 10001 10001" > /etc/sysctl.d/argus.conf

USER argus
EXPOSE 6767
ENTRYPOINT ["/usr/local/bin/argus"]
CMD ["run", "-config", "/etc/argus/argus.yaml", "-probes", "/etc/argus/probes.d"]
