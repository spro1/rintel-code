from typing import Optional

from .models import User

# in-memory storage
_USERS = {}


class Repo:
    def find(self, user_id: int) -> Optional[User]:
        return _USERS.get(user_id)

    def save(self, user: User) -> None:
        if not user.name or user.id < 0:
            raise ValueError("invalid user")
        _USERS[user.id] = user


class CachedRepo(Repo):
    def __init__(self):
        self.hits = 0

    def find(self, user_id: int) -> Optional[User]:
        self.hits += 1
        return super().find(user_id)
