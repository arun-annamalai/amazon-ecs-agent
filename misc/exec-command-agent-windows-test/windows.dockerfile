# escape=`

FROM golang:1.12 as build-env
MAINTAINER Amazon Web Services, Inc.

# There is dockerfile documentation on how to treat windows paths
WORKDIR C:\Users\Administrator\go\src\sleep
COPY ./sleep C:/Users/Administrator/go/src/sleep
RUN go build -tags integration -installsuffix cgo -a -o /go/bin/sleep .

WORKDIR C:\Users\Administrator\go\src\kill
ADD ./kill C:/Users/Administrator/go/src/kill
RUN CGO_ENABLED=0 go build -tags integration -installsuffix cgo -a -o /go/bin/kill .

#FROM amazon-ecs-ftest-windows-base:make
#MAINTAINER Amazon Web Services, Inc.
#COPY --from=build-env C:/Users/Administrator/go/src/sleep C:/
#COPY --from=build-env C:/Users/Administrator/go/src/kill C:/
