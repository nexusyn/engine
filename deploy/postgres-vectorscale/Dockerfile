# Postgres 17 do engine = pgvector/pgvector:pg17 (MESMO uid 999 / volume nexus_pgdata)
# + pgvectorscale 0.9.0 (DiskANN). Mantém compat com o volume existente — NÃO usar
# timescaledb-ha (uid 1000 quebra o data dir). Build a imagem a partir desta pasta.
#
#   docker build -t nexus-pg-vectorscale:pg17 .
#
# Depois: CREATE EXTENSION vectorscale; + índice USING diskann (embedding vector_cosine_ops).
FROM pgvector/pgvector:pg17
USER root
RUN apt-get update && apt-get install -y --no-install-recommends curl unzip ca-certificates && \
    curl -fsSL -o /tmp/vs.zip https://github.com/timescale/pgvectorscale/releases/download/0.9.0/pgvectorscale-0.9.0-pg17-amd64.zip && \
    echo "4cc8e0f233bf2d34d460efcd01bf91e11c36969ddc7c6ef06af933d817294429  /tmp/vs.zip" | sha256sum -c - && \
    unzip /tmp/vs.zip -d /tmp/vs && \
    dpkg -i /tmp/vs/pgvectorscale-postgresql-17_0.9.0-Linux_amd64.deb && \
    rm -rf /tmp/vs /tmp/vs.zip && \
    apt-get purge -y curl unzip && apt-get autoremove -y && rm -rf /var/lib/apt/lists/*
