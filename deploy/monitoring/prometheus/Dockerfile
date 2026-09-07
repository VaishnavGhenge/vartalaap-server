FROM prom/prometheus:v3.13.2
USER root
COPY prometheus.yml rules.yml /etc/prometheus/
COPY start.sh /start.sh
ENTRYPOINT ["/bin/sh", "/start.sh"]
