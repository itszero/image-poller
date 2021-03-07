image-poller
============

This little tool can be run as a cron job on your kubernetes cluster. It will monitor if the container image referenced
by a deployment has changed, and perform a rolling restart for those deployment that has changed.

This probably not super useful for most people. I made this because some container I run on my cluster are published
automatically on Github packages. I want them to be restarted whenever a new version is released but I don't want to put
the credentials to my cluster on github. Therefore, I made this tool to periodically check-in with Github instead.

## Usage

Set the following environment variables:

| Name          | Description                                                                                                         |
| ------------- | ------------------------------------------------------------------------------------------------------------------- |
| ENV           | Set to `PROD` for in-cluster authentication. It will read from `~/.kube/config` otherwise.                          |
| DOCKER_CONFIG | Can be omitted. This is used when you need to authenticate with your docker registry, for example, Github packages. |
| CHECKS        | comma-delimited string for all deployment names that needs to be checked.                                           |
