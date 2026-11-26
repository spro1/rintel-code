import type { Store, User } from "./store";

export class UserService {
  constructor(private store: Store, private notify: (message: string) => void) {}

  create(name: string): User {
    const user = { id: 1, name };
    this.store.save(user);
    this.notify(describe(user));
    return user;
  }

  lookup(id: number): User | undefined {
    return this.store.find(id);
  }
}

export function describe(user: User): string {
  const shout = (s: string) => s.toUpperCase();
  return shout(user.name);
}
