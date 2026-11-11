# Copyright 2025 The OpenSandbox Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from opensandbox_server.config import AppConfig, IngressConfig, RuntimeConfig, ServerConfig
from opensandbox_server.middleware.auth import AuthMiddleware


def _app_config_with_api_key() -> AppConfig:
    return AppConfig(
        server=ServerConfig(api_key="secret-key"),
        runtime=RuntimeConfig(type="docker", execd_image="opensandbox/execd:latest"),
        ingress=IngressConfig(mode="direct"),
    )


def _build_test_app():
    app = FastAPI()
    config = _app_config_with_api_key()
    app.add_middleware(AuthMiddleware, config=config)

    @app.get("/secured")
    def secured_endpoint():
        return {"ok": True}

    return app


def test_auth_middleware_rejects_missing_key():
    app = _build_test_app()
    client = TestClient(app)
    response = client.get("/secured")
    assert response.status_code == 401
    assert response.json()["code"] == "MISSING_API_KEY"


def test_auth_middleware_accepts_valid_key():
    app = _build_test_app()
    client = TestClient(app)
    response = client.get("/secured", headers={"OPEN-SANDBOX-API-KEY": "secret-key"})
    assert response.status_code == 200
    assert response.json() == {"ok": True}


@pytest.mark.parametrize("prefix", ["", "/v1"])
def test_auth_middleware_requires_key_for_proxy_to_sandbox(prefix: str):
    """A sandbox can reach the server's proxy route, so without a key one sandbox could drive another."""
    app = _build_test_app()

    @app.get(prefix + "/sandboxes/{sandbox_id}/proxy/{port}/{full_path:path}")
    def proxy_echo(sandbox_id: str, port: int, full_path: str):
        return {"proxied": True}

    client = TestClient(app)
    path = f"{prefix}/sandboxes/abc-123/proxy/8080/foo/bar"

    response = client.get(path)
    assert response.status_code == 401
    assert response.json()["code"] == "MISSING_API_KEY"

    response = client.get(path, headers={"OPEN-SANDBOX-API-KEY": "wrong-key"})
    assert response.status_code == 401
    assert response.json()["code"] == "INVALID_API_KEY"

    response = client.get(path, headers={"OPEN-SANDBOX-API-KEY": "secret-key"})
    assert response.status_code == 200
    assert response.json()["proxied"] is True


def test_auth_middleware_proxy_requires_auth_in_multi_tenant_mode():
    """In multi-tenant mode, proxy paths must require auth to establish tenant context."""
    from unittest.mock import MagicMock

    from opensandbox_server.tenants.models import TenantEntry

    app = FastAPI()
    config = _app_config_with_api_key()
    mock_provider = MagicMock()
    tenant = TenantEntry(name="tenant-a", namespace="ns-a", api_keys=["key-a"])
    mock_provider.lookup.return_value = tenant
    app.add_middleware(AuthMiddleware, config=config, tenant_provider=mock_provider)

    @app.get("/sandboxes/{sandbox_id}/proxy/{port}/{full_path:path}")
    def proxy_echo(sandbox_id: str, port: int, full_path: str):
        return {"proxied": True}

    client = TestClient(app)

    response = client.get("/sandboxes/abc/proxy/8080/foo")
    assert response.status_code == 401, "proxy must require auth in multi-tenant mode"

    response = client.get(
        "/sandboxes/abc/proxy/8080/foo",
        headers={"OPEN-SANDBOX-API-KEY": "key-a"},
    )
    assert response.status_code == 200
