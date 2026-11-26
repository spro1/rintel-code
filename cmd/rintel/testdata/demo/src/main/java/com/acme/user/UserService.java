package com.acme.user;

import com.acme.mail.MailSender;
import lombok.RequiredArgsConstructor;
import org.springframework.stereotype.Service;

@Service
@RequiredArgsConstructor
public class UserService {
    private final UserRepository userRepository;
    private final MailSender mailSender;

    public User createUser(String email) {
        User user = new User();
        user.setEmail(email);
        userRepository.save(user);
        mailSender.send(user.getEmail(), "welcome");
        return user;
    }

    public User find(String email) {
        return userRepository.findByEmail(email);
    }

    Object reflect(java.lang.reflect.Method m) throws Exception {
        return m.invoke(this);
    }
}
