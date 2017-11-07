FROM golang

RUN go get github.com/gwoo/kala
ENTRYPOINT kala run
EXPOSE 8000
