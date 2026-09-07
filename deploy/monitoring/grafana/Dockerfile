FROM grafana/grafana:13.2.0
USER root
COPY provisioning /etc/grafana/provisioning
COPY dashboards /etc/grafana/dashboards
COPY start.sh /start.sh
ENTRYPOINT ["/bin/sh", "/start.sh"]
