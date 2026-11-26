from app.repo import Repo
from app.service import UserService


def test_create():
    assert UserService(Repo(), lambda s: None).create("ann").name == "ann"
