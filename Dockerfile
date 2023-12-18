FROM golang:1.21-alpine AS build
COPY . $GOPATH/src/app
WORKDIR $GOPATH/src/app
RUN CGO_ENABLED=0 GOOS=linux go build \
  -a -installsuffix cgo -o app .

FROM scratch
COPY --from=build /go/src/app/app /bin/app
EXPOSE 3000
CMD ["app"]
