# stanzas

Import stanzas for golang

The following is an addition to goimports by allowing the grouping of imports
into three groups (stanzas); stdlib, 3rd party libs and then your own project.

This is especially useful for working with larger projects like [juju](https://github.com/juju/juju)

[![Screencast](https://img.youtube.com/vi/EguALVNLwjM/0.jpg)](https://www.youtube.com/watch?v=EguALVNLwjM)

## Setup

It should run in the same way as goimports, so swapping out goimports to stanzas
once you've installed it into your `$PATH`.

Locally I've setup `$GOPATH/bin` to be included in my `$PATH`, so running

```
go get -u github.com/SimonRichardson/stanzas
```

Then follow the screencast above.

## Issues

If you have created your own stanzas then it will not merge those but attempt to
work along side them, this is similar to how gofmt and goimports also work.