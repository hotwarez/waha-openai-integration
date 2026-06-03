FROM golang:alpine AS builder
WORKDIR /app
RUN apk add --no-cache git
COPY go.mod ./
COPY main.go ./
RUN go get ./... && go mod tidy
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /waha-openai .

FROM alpine:latest
RUN apk --no-cache add ca-certificates
COPY --from=builder /waha-openai /waha-openai
EXPOSE 8080
CMD ["/waha-openai"]
