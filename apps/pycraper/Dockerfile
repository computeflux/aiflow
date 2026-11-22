# pycraper — X/Reddit/GitHub/小红书抓取 worker（Temporal activity worker + MySQL 落库）
# 构建：docker build -t pycraper apps/pycraper
FROM python:3.12-slim
ARG PIP_INDEX_URL=https://pypi.tuna.tsinghua.edu.cn/simple
ENV PIP_INDEX_URL=${PIP_INDEX_URL}

WORKDIR /app

# Playwright 浏览器安装到 /ms-playwright：默认位置在 /root/.cache，
# 会被下面的 rm -rf /root/.cache 误删（且 ENV 需在安装前生效）
ENV PLAYWRIGHT_BROWSERS_PATH=/ms-playwright

# 依赖（版本区间与 pyproject.toml / hacks/build.sh 保持一致）+ Playwright Chromium
RUN apt-get update && apt-get install -y --no-install-recommends git \
    && apt-get install -y --no-install-recommends libatomic1 \
    && rm -rf /var/lib/apt/lists/* \
    && pip install --no-cache-dir \
        "crawl4ai>=0.9,<1.0" \
        "mcp[cli]>=1.0,<2.0" \
        "opentelemetry-api>=1.24,<2.0" \
        "opentelemetry-sdk>=1.24,<2.0" \
        "opentelemetry-exporter-otlp-proto-grpc>=1.24,<2.0" \
        "pydantic>=2.0,<3.0" \
        "pymysql>=1.1,<2.0" \
        "python-dotenv>=1.0,<2.0" \
        "requests>=2.31,<3.0" \
        "temporalio>=1.0,<2.0" \
    && python -m playwright install --with-deps chromium \
    && rm -rf /root/.cache /var/lib/apt/lists/*

COPY . .

CMD ["python3", "main.py"]
