from app.models import User
from app.repo import Repo


class UserService:
    def __init__(self, repo: Repo, notify):
        self.repo = repo
        self.notify = notify

    def create(self, name: str) -> User:
        user = User(1, name)
        self.repo.save(user)
        self.notify(user.display())
        return user

    def lookup(self, user_id):
        return self.repo.find(user_id)


def describe(thing):
    return thing.display()
