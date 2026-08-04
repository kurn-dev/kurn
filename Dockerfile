# kurnd — static binary on a distroless base. The daemon is crash-only:
# no graceful-shutdown hook is needed (SIGKILL is a legal stop; recovery is
# journal replay + the artifact fast path at the next start).
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /kurnd ./cmd/kurnd

FROM gcr.io/distroless/static-debian12
COPY --from=build /kurnd /kurnd
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/kurnd", "-data", "/data"]
