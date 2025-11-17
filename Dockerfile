FROM python:3.10.14-bookworm

COPY --from=ghcr.io/astral-sh/uv:latest /uv /uvx /bin/

RUN apt update --allow-unauthenticated && \
    apt install -y curl unzip && \
    curl "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip" -o "awscliv2.zip" && \
    unzip awscliv2.zip && \
    ./aws/install

COPY src src
COPY README.md README.md
COPY uv.lock uv.lock
COPY pyproject.toml pyproject.toml

RUN uv sync

COPY app.py app.py

CMD ["uv", "run", "marimo", "run", "app.py", "--host=0.0.0.0", "--port=2720"]
