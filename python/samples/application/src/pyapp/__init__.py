import os

from fastapi import FastAPI, HTTPException
from pydantic import BaseModel
from pylib import capitalize, clamp, fibonacci, is_palindrome, is_prime, reverse

app = FastAPI(title="Py Example Application", version="0.1.0")


class StringInput(BaseModel):
    text: str


class ClampInput(BaseModel):
    value: float
    min_val: float
    max_val: float


@app.get("/")
def health():
    return {"status": "ok", "service": "py_example_application"}


@app.get("/strings/reverse/{text}")
def reverse_string(text: str):
    return {"input": text, "result": reverse(text)}


@app.get("/strings/capitalize/{text}")
def capitalize_string(text: str):
    return {"input": text, "result": capitalize(text)}


@app.get("/strings/palindrome/{text}")
def check_palindrome(text: str):
    return {"input": text, "is_palindrome": is_palindrome(text)}


@app.get("/math/fibonacci/{n}")
def get_fibonacci(n: int):
    if n < 0 or n > 100:
        raise HTTPException(status_code=400, detail="n must be between 0 and 100")
    return {"n": n, "sequence": fibonacci(n)}


@app.get("/math/prime/{n}")
def check_prime(n: int):
    if n < 0 or n > 1_000_000:
        raise HTTPException(status_code=400, detail="n must be between 0 and 1000000")
    return {"n": n, "is_prime": is_prime(n)}


@app.post("/math/clamp")
def clamp_value(body: ClampInput):
    return {
        "value": body.value,
        "min": body.min_val,
        "max": body.max_val,
        "result": clamp(body.value, body.min_val, body.max_val),
    }


def main():
    import uvicorn

    host = "0.0.0.0"
    port = int(os.environ.get("PORT", "3930"))
    url_host = "127.0.0.1" if host == "0.0.0.0" else host
    print(f"Server running at http://{url_host}:{port}")
    uvicorn.run(app, host=host, port=port)


if __name__ == "__main__":
    main()
