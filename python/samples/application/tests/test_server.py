from fastapi.testclient import TestClient
from pyapp import app

client = TestClient(app)


def test_health():
    response = client.get("/")
    assert response.status_code == 200
    data = response.json()
    assert data["status"] == "ok"
    assert data["service"] == "py_example_application"


def test_reverse_string():
    response = client.get("/strings/reverse/hello")
    assert response.status_code == 200
    data = response.json()
    assert data["input"] == "hello"
    assert data["result"] == "olleh"


def test_capitalize_string():
    response = client.get("/strings/capitalize/hello")
    assert response.status_code == 200
    data = response.json()
    assert data["result"] == "Hello"


def test_palindrome_true():
    response = client.get("/strings/palindrome/racecar")
    assert response.status_code == 200
    data = response.json()
    assert data["is_palindrome"] is True


def test_palindrome_false():
    response = client.get("/strings/palindrome/hello")
    assert response.status_code == 200
    data = response.json()
    assert data["is_palindrome"] is False


def test_fibonacci():
    response = client.get("/math/fibonacci/5")
    assert response.status_code == 200
    data = response.json()
    assert data["sequence"] == [0, 1, 1, 2, 3]


def test_fibonacci_invalid():
    response = client.get("/math/fibonacci/-1")
    assert response.status_code == 400


def test_prime():
    response = client.get("/math/prime/17")
    assert response.status_code == 200
    data = response.json()
    assert data["is_prime"] is True


def test_prime_false():
    response = client.get("/math/prime/4")
    assert response.status_code == 200
    data = response.json()
    assert data["is_prime"] is False


def test_prime_invalid():
    response = client.get("/math/prime/-1")
    assert response.status_code == 400


def test_prime_out_of_range():
    response = client.get("/math/prime/1000001")
    assert response.status_code == 400


def test_clamp():
    response = client.post(
        "/math/clamp", json={"value": 15, "min_val": 0, "max_val": 10}
    )
    assert response.status_code == 200
    data = response.json()
    assert data["result"] == 10
