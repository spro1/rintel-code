// Users and where they are kept.
export type User = { id: number; name: string };

export interface Store {
  find(id: number): User | undefined;
  save(user: User): void;
}

export class MemoryStore implements Store {
  private users = new Map<number, User>();

  find(id: number): User | undefined {
    return this.users.get(id);
  }

  save(user: User): void {
    if (!user.name || user.id < 0) {
      throw new Error("invalid user");
    }
    this.users.set(user.id, user);
  }
}
