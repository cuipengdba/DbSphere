FROM postgres:18-bookworm

RUN apt-get update && apt-get install -y --no-install-recommends pgbackrest \
    && rm -rf /var/lib/apt/lists/*
COPY test/recovery-drill/pgbackrest.conf /etc/dbsphere-pgbackrest.conf
COPY test/recovery-drill/init.sh /docker-entrypoint-initdb.d/01-repository.sh
RUN chmod 0755 /docker-entrypoint-initdb.d/01-repository.sh
