# pyreplay — X 点赞/执行 worker（Temporal activity worker + 后台 like_jobs 执行器）
# 构建：docker build -t pyreplay apps/pyreplay
FROM python:3.12-slim
ARG PIP_INDEX_URL=https://pypi.tuna.tsinghua.edu.cn/simple
ENV PIP_INDEX_URL=${PIP_INDEX_URL}

WORKDIR /app

# Playwright 浏览器安装到 /ms-playwright：默认位置在 /root/.cache，
# 会被下面的 rm -rf /root/.cache 误删（且 ENV 需在安装前生效）
ENV PLAYWRIGHT_BROWSERS_PATH=/ms-playwright

# 依赖（版本区间与 pyproject.toml 保持一致）+ Playwright Chromium
# libatomic1：playwright driver node 需要（基础镜像 slim 不带，--with-deps 不会装）
RUN apt-get update && apt-get install -y --no-install-recommends libatomic1 && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir \
        "crawl4ai>=0.9,<1.0" \
        "opentelemetry-api>=1.24,<2.0" \
        "opentelemetry-sdk>=1.24,<2.0" \
        "opentelemetry-exporter-otlp-proto-grpc>=1.24,<2.0" \
        "pydantic>=2.0,<3.0" \
        "pymysql>=1.1,<2.0" \
        "redis>=5.0,<6.0" \
        "python-dotenv>=1.0,<2.0" \
        "temporalio>=1.0,<2.0" \
    && python -m playwright install --with-deps chromium \
    && rm -rf /root/.cache /var/lib/apt/lists/*

COPY . .

# 入口：后台起 'like' skill server（9500）+ 前台 Temporal worker（见 entrypoint.sh）
CMD ["sh", "entrypoint.sh"]
