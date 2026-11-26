package com.acme.user;

import org.springframework.web.bind.annotation.*;

@RestController
@RequestMapping("/api/users")
public class UserController {
    private final UserService userService;

    public UserController(UserService userService) {
        this.userService = userService;
    }

    @PostMapping
    public User create(@RequestBody String email) {
        return userService.createUser(email);
    }

    @GetMapping("/{email}")
    public User get(@PathVariable String email) {
        return userService.find(email);
    }
}
