FROM sunpeek/poetry:py3.10-slim

RUN apt update --allow-unauthenticated && \
    apt install -y curl unzip && \
    curl "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip" -o "awscliv2.zip" && \
    unzip awscliv2.zip && \
    ./aws/install

COPY aws_config /root/.aws/config
COPY src src
COPY README.md README.md
COPY app.py app.py
COPY poetry.lock poetry.lock
COPY pyproject.toml pyproject.toml

RUN poetry install

CMD ["poetry", "run", "marimo", "run", "app.py", "--host=0.0.0.0", "--port=2720"]
# CMD ["poetry", "run", "marimo", "edit", "app.py", "--host=0.0.0.0", "--port=2720", "--no-token"]
