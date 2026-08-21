FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
COPY static ./static
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/targets-admin .

FROM alpine:3.21
COPY --from=build /out/targets-admin /targets-admin
# Runs as root: it must write into a bind-mounted host directory
# (./prometheus/targets) whose ownership isn't known ahead of time.
ENTRYPOINT ["/targets-admin"]
