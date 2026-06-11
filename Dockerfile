FROM golang:1.26-alpine3.24 AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o offhours-guard main.go

FROM alpine:3.24
RUN apk --no-cache add ca-certificates tzdata
WORKDIR /
COPY --from=builder /app/offhours-guard /offhours-guard
EXPOSE 8082 8083
ENTRYPOINT ["/offhours-guard"]