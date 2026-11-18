ARG PG_MAJOR=18
FROM postgres:${PG_MAJOR}-bookworm
ARG PG_MAJOR

RUN apt-get update && apt-get install -y --no-install-recommends python3 python3-venv \
    && python3 -m venv /opt/patroni \
    && /opt/patroni/bin/pip install --no-cache-dir 'patroni[etcd3,psycopg3]==4.1.5' \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /var/lib/postgresql/patroni \
    && chown -R postgres:postgres /var/lib/postgresql/patroni \
    && chmod 700 /var/lib/postgresql/patroni

COPY test/patroni-drill/entrypoint.sh /usr/local/bin/dbsphere-patroni-entrypoint.sh
RUN chmod +x /usr/local/bin/dbsphere-patroni-entrypoint.sh

USER postgres
ENV PATH=/opt/patroni/bin:/usr/lib/postgresql/${PG_MAJOR}/bin:$PATH \
    PATRONI_PG_MAJOR=${PG_MAJOR}
ENTRYPOINT ["/usr/local/bin/dbsphere-patroni-entrypoint.sh"]
