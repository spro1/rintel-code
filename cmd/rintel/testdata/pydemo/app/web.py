from fastapi import FastAPI

from app.repo import CachedRepo
from app.service import UserService

app = FastAPI()
service = UserService(CachedRepo(), print)


@app.get("/users/{user_id}")
def get_user(user_id: int):
    return service.lookup(user_id)


@app.post("/users")
def create_user(name: str):
    fmt = lambda u: u.name
    return fmt(service.create(name))
