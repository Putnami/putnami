from fastapi import FastAPI
import uvicorn

app = FastAPI()


@app.get("/")
def health():
    return {"Hello": "World"}


def main():
    host = "0.0.0.0"
    port = 8000
    url_host = "127.0.0.1" if host == "0.0.0.0" else host
    print(f"Server running at http://{url_host}:{port}")
    uvicorn.run(app, host=host, port=port)


if __name__ == "__main__":
    main()
