FROM golang:1.24-alpine AS build
WORKDIR /src
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway

FROM scratch
WORKDIR /
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/gateway /gateway
COPY configs/models.json /configs/models.json
ENV HOME=/home/gateway
ENV LISTEN_ADDR=:8080
ENV MODEL_CONFIG_FILE=/configs/models.json
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/gateway"]
