"""Domain objects."""
from dataclasses import dataclass


@dataclass
class User:
    id: int
    name: str

    def display(self) -> str:
        return self.name.title()
